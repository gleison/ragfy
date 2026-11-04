package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	llama "github.com/mutablelogic/go-llama/pkg/llamacpp"
	"github.com/mutablelogic/go-llama/pkg/llamacpp/schema"
	llamasys "github.com/mutablelogic/go-llama/sys/llamacpp"
)

const (
	chatModel         = "Qwen3-4B-Q4_K_M.gguf"
	chatURL           = "hf://Qwen/Qwen3-4B-GGUF/Qwen3-4B-Q4_K_M.gguf"
	chatSHA256        = "7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5"
	embedModel        = "Qwen3-Embedding-0.6B-Q8_0.gguf"
	embedURL          = "hf://Qwen/Qwen3-Embedding-0.6B-GGUF/Qwen3-Embedding-0.6B-Q8_0.gguf"
	embedSHA256       = "06507c7b42688469c4e7298b0a1e16deff06caf291cf0a5b278c308249c3e439"
	EmbeddingVersion  = embedSHA256 + ":query-instruction-v1"
	chatContextSize   = 4096
	chatBatchSize     = 2048
	chatOutputTokens  = 1200
	embedContextSize  = 512
	maximumEmbedChars = 1400
)

type Llama struct {
	runtime   *llama.Llama
	chatName  string
	embedName string
}

var _ Embedder = (*Llama)(nil)
var _ Generator = (*Llama)(nil)

func NewLlama(ctx context.Context, directory string) (*Llama, error) {
	return newLlama(ctx, directory, modelSpecifications(), []string{chatModel, embedModel}, chatModel)
}

func NewEmbeddingLlama(ctx context.Context, directory string) (*Llama, error) {
	return newLlama(ctx, directory, []modelSpecification{embeddingModelSpecification()}, []string{embedModel}, "")
}

type modelSpecification struct {
	name   string
	url    string
	sha256 string
}

func newLlama(ctx context.Context, directory string, specifications []modelSpecification, loadNames []string, chatName string) (*Llama, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	runtime, err := llama.New(directory)
	if err != nil {
		return nil, err
	}
	engine := &Llama{runtime: runtime, chatName: chatName, embedName: embedModel}
	for _, specification := range specifications {
		modelFile, modelErr := runtime.GetModel(ctx, specification.name)
		if modelErr == nil {
			modelPath := filepath.Join(runtime.Store.Path(), modelFile.Path)
			if verificationErr := verifyFile(modelPath, specification.sha256); verificationErr != nil {
				_ = os.Remove(modelPath)
				modelErr = verificationErr
			}
		}
		if modelErr != nil {
			log.Printf("startup: downloading model %s", specification.name)
			lastPercentage := -1
			modelFile, err = runtime.PullModel(ctx, schema.PullModelRequest{URL: specification.url}, func(_ string, downloaded, total uint64) error {
				if total > 0 {
					percentage := int(downloaded * 100 / total)
					if percentage > lastPercentage {
						lastPercentage = percentage
						log.Printf("startup: download %s: %d%%", specification.name, percentage)
					}
				}
				return nil
			})
			if err != nil {
				runtime.Close()
				return nil, fmt.Errorf("download %s: %w", specification.name, err)
			}
		}
		modelPath := filepath.Join(runtime.Store.Path(), modelFile.Path)
		if err = verifyFile(modelPath, specification.sha256); err != nil {
			_ = os.Remove(modelPath)
			runtime.Close()
			return nil, fmt.Errorf("modelo %s inválido: %w", specification.name, err)
		}
		log.Printf("startup: verified model %s", specification.name)
	}
	for _, name := range loadNames {
		log.Printf("startup: loading model %s", name)
		if _, err := runtime.LoadModel(ctx, schema.LoadModelRequest{Name: name}); err != nil {
			runtime.Close()
			return nil, fmt.Errorf("load %s: %w", name, err)
		}
	}
	log.Println("startup: models loaded")
	return engine, nil
}

func (engine *Llama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	for index := range texts {
		texts[index] = truncate(texts[index], maximumEmbedChars)
	}
	embeddings := true
	contextSize := uint32(embedContextSize)
	request := schema.ContextRequest{LoadModelRequest: schema.LoadModelRequest{Name: engine.embedName}, ContextSize: &contextSize, BatchSize: &contextSize, UBatchSize: &contextSize, Embeddings: &embeddings}
	var vectors [][]float32
	for index := range texts {
		err := engine.runtime.WithContext(ctx, request, func(_ context.Context, task *llama.Task) error {
			task.CachedModel().Lock()
			defer task.CachedModel().Unlock()
			if pooling := task.Context().PoolingType(); pooling != llamasys.PoolingLast {
				return fmt.Errorf("embedding model pooling must be 'last', got %s", pooling)
			}
			options := llamasys.DefaultEmbeddingOptions()
			options.Normalize = true
			batch, err := task.Context().ComputeEmbeddings(task.Model(), texts[index:index+1], options)
			if err != nil {
				return err
			}
			vectors = append(vectors, batch.Embeddings...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return vectors, nil
}

func (engine *Llama) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	queryInput := "Instruct: Given a coding assistant prompt, retrieve relevant passages from the user's knowledge base that help answer the prompt.\nQuery: " + query
	vectors, err := engine.Embed(ctx, []string{queryInput})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (engine *Llama) Generate(ctx context.Context, prompt string) (string, error) {
	contextSize := uint32(chatContextSize)
	batchSize := uint32(chatBatchSize)
	generatedText := ""
	err := engine.runtime.WithContext(ctx, schema.ContextRequest{
		LoadModelRequest: schema.LoadModelRequest{Name: engine.chatName},
		ContextSize:      &contextSize,
		BatchSize:        &batchSize,
		UBatchSize:       &batchSize,
	}, func(_ context.Context, task *llama.Task) error {
		task.CachedModel().Lock()
		defer task.CachedModel().Unlock()
		tokenOptions := llamasys.DefaultTokenizeOptions()
		tokenOptions.AddSpecial = true
		tokenOptions.ParseSpecial = true
		tokens, err := task.Model().Tokenize(qwenChatMLPrompt(prompt), tokenOptions)
		if err != nil {
			return err
		}
		if len(tokens)+chatOutputTokens > int(contextSize) {
			return fmt.Errorf("digest prompt exceeds context budget: %d tokens", len(tokens))
		}
		if err := decodePrompt(task, tokens, int(batchSize)); err != nil {
			return err
		}
		generatedText, err = sampleResponse(ctx, task, tokens)
		return err
	})
	return generatedText, err
}

func (engine *Llama) Close() error {
	return engine.runtime.Close()
}

func modelSpecifications() []modelSpecification {
	return []modelSpecification{{chatModel, chatURL, chatSHA256}, embeddingModelSpecification()}
}

func embeddingModelSpecification() modelSpecification {
	return modelSpecification{embedModel, embedURL, embedSHA256}
}

func decodePrompt(task *llama.Task, tokens []llamasys.Token, batchSize int) error {
	for _, bounds := range tokenChunks(len(tokens), batchSize) {
		lastBatch := bounds[1] == len(tokens)
		batch, err := llamasys.BatchFromTokens(tokens[bounds[0]:bounds[1]], int32(bounds[0]), 0, lastBatch)
		if err != nil {
			return err
		}
		err = batch.Decode(task.Context())
		batch.Close()
		if err != nil {
			return fmt.Errorf("digest prompt decode at token %d: %w", bounds[0], err)
		}
	}
	return nil
}

func sampleResponse(ctx context.Context, task *llama.Task, promptTokens []llamasys.Token) (string, error) {
	parameters := llamasys.DefaultSamplerParams()
	parameters.Temperature, parameters.TopP, parameters.TopK = 0.7, 0.8, 20
	sampler, err := llamasys.NewSampler(task.Model(), parameters)
	if err != nil {
		return "", err
	}
	defer sampler.Close()
	var output strings.Builder
	for index := 0; index < chatOutputTokens; index++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		token, err := sampler.Sample(task.Context(), -1)
		if err != nil {
			return "", err
		}
		if task.Model().IsEOG(token) {
			return output.String(), nil
		}
		piece, err := task.Model().Detokenize([]llamasys.Token{token}, llamasys.DefaultDetokenizeOptions())
		if err != nil {
			return "", err
		}
		output.WriteString(piece)
		nextBatch, err := llamasys.BatchFromTokens([]llamasys.Token{token}, int32(len(promptTokens)+index), 0, true)
		if err != nil {
			return "", err
		}
		err = nextBatch.Decode(task.Context())
		nextBatch.Close()
		if err != nil {
			return "", fmt.Errorf("digest token decode: %w", err)
		}
	}
	return "", errors.New("digest output exceeded token budget")
}

func qwenChatMLPrompt(userPrompt string) string {
	return "<|im_start|>user\n" + userPrompt + "<|im_end|>\n<|im_start|>assistant\n<think>\n\n</think>\n\n"
}

func tokenChunks(total, size int) [][2]int {
	if total <= 0 || size <= 0 {
		return nil
	}
	chunks := make([][2]int, 0, (total+size-1)/size)
	for start := 0; start < total; start += size {
		chunks = append(chunks, [2]int{start, min(start+size, total)})
	}
	return chunks
}

func verifyFile(path, expectedSHA256 string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if actualSHA256 := hex.EncodeToString(hash.Sum(nil)); actualSHA256 != expectedSHA256 {
		return fmt.Errorf("SHA-256 mismatch: got %s", actualSHA256)
	}
	return nil
}

func truncate(text string, maximumLength int) string {
	if len(text) <= maximumLength {
		return text
	}
	for maximumLength > 0 && maximumLength < len(text) && !utf8.RuneStart(text[maximumLength]) {
		maximumLength--
	}
	return text[:maximumLength]
}
