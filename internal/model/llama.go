package model

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	llama "github.com/mutablelogic/go-llama/pkg/llamacpp"
	"github.com/mutablelogic/go-llama/pkg/llamacpp/schema"
	"github.com/mutablelogic/go-llama/pkg/llamacpp/store"
	llamasys "github.com/mutablelogic/go-llama/sys/llamacpp"

	appconfig "ragfy/internal/config"
)

const (
	chatContextSize   = 32768
	chatBatchSize     = 2048
	chatOutputTokens  = 4096
	embedContextSize  = 512
	maximumEmbedChars = 1400
)

type Llama struct {
	runtime          *llama.Llama
	chatName         string
	chatSHA256       string
	embedName        string
	embedSHA256      string
	pooling          string
	queryInstruction string
}

var _ Embedder = (*Llama)(nil)
var _ Generator = (*Llama)(nil)

func NewLlama(ctx context.Context, directory string) (*Llama, error) {
	config := appconfig.Default()
	generation := appconfig.DefaultGenerationConfig()
	config.Generation = &generation
	return NewLlamaWithConfig(ctx, directory, config)
}

func NewEmbeddingLlama(ctx context.Context, directory string) (*Llama, error) {
	return NewEmbeddingLlamaWithConfig(ctx, directory, appconfig.DefaultEmbeddingConfig())
}

func NewEmbeddingLlamaWithConfig(ctx context.Context, directory string, config appconfig.EmbeddingConfig) (*Llama, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return newLlama(ctx, directory, config, nil)
}

func NewLlamaWithConfig(ctx context.Context, directory string, config appconfig.Config) (*Llama, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return newLlama(ctx, directory, config.Embedding, config.Generation)
}

type modelSpecification struct {
	name   string
	url    string
	sha256 string
}

func newLlama(ctx context.Context, directory string, embedConfig appconfig.EmbeddingConfig, generation *appconfig.GenerationConfig) (*Llama, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	runtime, err := llama.New(directory)
	if err != nil {
		return nil, err
	}
	engine := &Llama{runtime: runtime, embedName: embedConfig.Name(), pooling: embedConfig.Pooling, queryInstruction: embedConfig.QueryInstruction}
	specifications := []modelSpecification{{engine.embedName, embedConfig.URI, embedConfig.SHA256}}
	if generation != nil {
		client, err := store.NewClient()
		if err == nil {
			engine.chatName, err = client.GetDestPath(generation.URI)
		}
		if err != nil {
			runtime.Close()
			return nil, fmt.Errorf("resolve local generation model path: %w", err)
		}
		specifications = append(specifications, modelSpecification{engine.chatName, generation.URI, generation.SHA256})
	}
	for _, specification := range specifications {
		actualSHA256, err := prepareModel(ctx, runtime, specification)
		if err != nil {
			runtime.Close()
			return nil, err
		}
		if specification.name == engine.embedName {
			engine.embedSHA256 = actualSHA256
		} else {
			engine.chatSHA256 = actualSHA256
		}
	}
	log.Println("startup: models loaded")
	return engine, nil
}

func prepareModel(ctx context.Context, runtime *llama.Llama, specification modelSpecification) (string, error) {
	checksum, err := cachedModelChecksum(ctx, runtime, specification)
	if err != nil {
		checksum, err = downloadModel(ctx, runtime, specification)
		if err != nil {
			return "", err
		}
	}
	log.Printf("startup: verified model %s", specification.name)
	log.Printf("startup: loading model %s", specification.name)
	if _, err := runtime.LoadModel(ctx, schema.LoadModelRequest{Name: specification.name}); err != nil {
		return "", fmt.Errorf("load %s: %w", specification.name, err)
	}
	return checksum, nil
}

func cachedModelChecksum(ctx context.Context, runtime *llama.Llama, specification modelSpecification) (string, error) {
	modelFile, err := runtime.GetModel(ctx, specification.name)
	if err != nil {
		return "", err
	}
	return verifyModelOrRemove(runtime.Store.Path(), modelFile.Path, specification.sha256)
}

func downloadModel(ctx context.Context, runtime *llama.Llama, specification modelSpecification) (string, error) {
	log.Printf("startup: downloading model %s", specification.name)
	lastPercentage := -1
	modelFile, err := runtime.PullModel(ctx, schema.PullModelRequest{URL: specification.url}, func(_ string, downloaded, total uint64) error {
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
		return "", fmt.Errorf("download %s: %w", specification.name, err)
	}
	checksum, err := verifyModelOrRemove(runtime.Store.Path(), modelFile.Path, specification.sha256)
	if err != nil {
		return "", fmt.Errorf("verify %s: %w", specification.name, err)
	}
	return checksum, nil
}

func verifyModelOrRemove(directory, filename, expectedChecksum string) (string, error) {
	modelPath := filepath.Join(directory, filename)
	checksum, err := verifiedSHA256(modelPath, expectedChecksum)
	if err != nil {
		_ = os.Remove(modelPath)
	}
	return checksum, err
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
			if pooling := task.Context().PoolingType(); engine.pooling != "" && engine.pooling != "auto" && pooling.String() != engine.pooling {
				return fmt.Errorf("embedding model pooling must be %q, got %s", engine.pooling, pooling)
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
	queryInput := engine.queryInstruction + query
	vectors, err := engine.Embed(ctx, []string{queryInput})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (engine *Llama) ValidateEmbedding(ctx context.Context) error {
	vectors, err := engine.Embed(ctx, []string{"Ragfy embedding validation.", "Semantic retrieval validation."})
	if err != nil {
		return err
	}
	if len(vectors) != 2 || len(vectors[0]) == 0 || len(vectors[0]) != len(vectors[1]) {
		return errors.New("embedding model returned inconsistent vector dimensions")
	}
	different := false
	for vectorIndex, vector := range vectors {
		norm := 0.0
		for _, value := range vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return errors.New("embedding model returned non-finite values")
			}
			norm += float64(value) * float64(value)
		}
		if norm == 0 {
			return errors.New("embedding model returned a zero vector")
		}
		if vectorIndex == 1 {
			for index := range vector {
				different = different || vector[index] != vectors[0][index]
			}
		}
	}
	if !different {
		return errors.New("embedding model returned identical vectors for different inputs")
	}
	return nil
}

func (engine *Llama) SHA256() string { return engine.embedSHA256 }

func (engine *Llama) GenerationSHA256() string { return engine.chatSHA256 }

func (engine *Llama) Generate(ctx context.Context, prompt string) (string, error) {
	if engine.chatName == "" {
		return "", errors.New("local generation model is not configured")
	}
	contextSize := uint32(chatContextSize)
	batchSize := uint32(chatBatchSize)
	generatedText := ""
	err := engine.runtime.WithContext(ctx, schema.ContextRequest{
		LoadModelRequest: schema.LoadModelRequest{Name: engine.chatName},
		ContextSize:      &contextSize,
		BatchSize:        &batchSize,
		UBatchSize:       &batchSize,
	}, func(ctx context.Context, task *llama.Task) error {
		task.CachedModel().Lock()
		defer task.CachedModel().Unlock()
		tokens, err := tokenizeChatPrompt(task.Model(), prompt)
		if err != nil {
			return err
		}
		if len(tokens)+chatOutputTokens > int(contextSize) {
			return fmt.Errorf("digest prompt exceeds context budget: %d tokens", len(tokens))
		}
		if err := decodePrompt(ctx, task, tokens, int(batchSize)); err != nil {
			return err
		}
		generatedText, err = sampleResponse(ctx, task, tokens)
		generatedText = llama.ParseReasoning(generatedText).Content
		return err
	})
	return generatedText, err
}

func tokenizeChatPrompt(chatModel *llamasys.Model, prompt string) ([]llamasys.Token, error) {
	const contentMarker = "RagfyPromptContent"
	if !chatModel.HasChatTemplate() {
		return nil, errors.New("local generation model has no chat template")
	}
	template, err := llamasys.ApplyTemplateWithModel(chatModel, "", []llamasys.ChatMessage{{Role: "user", Content: contentMarker}}, true)
	if err != nil {
		return nil, fmt.Errorf("apply local generation chat template: %w", err)
	}
	before, after, ok := strings.Cut(template, contentMarker)
	if !ok || strings.Contains(after, contentMarker) {
		return nil, errors.New("local generation model has an unsupported chat template")
	}
	tokenOptions := llamasys.DefaultTokenizeOptions()
	tokenOptions.AddSpecial = true
	tokenOptions.ParseSpecial = true
	prefix, err := chatModel.Tokenize(before, tokenOptions)
	if err != nil {
		return nil, err
	}
	tokenOptions.AddSpecial = false
	tokenOptions.ParseSpecial = false
	content, err := chatModel.Tokenize(prompt, tokenOptions)
	if err != nil {
		return nil, err
	}
	tokenOptions.ParseSpecial = true
	suffix, err := chatModel.Tokenize(after, tokenOptions)
	if err != nil {
		return nil, err
	}
	return append(append(prefix, content...), suffix...), nil
}

func (engine *Llama) Close() error {
	return engine.runtime.Close()
}

func decodePrompt(ctx context.Context, task *llama.Task, tokens []llamasys.Token, batchSize int) error {
	for _, bounds := range tokenChunks(len(tokens), batchSize) {
		if err := ctx.Err(); err != nil {
			return err
		}
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
	parameters.Temperature, parameters.TopP, parameters.TopK = 0.1, 0.8, 20
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

func verifiedSHA256(path, expectedSHA256 string) (string, error) {
	actualSHA256, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	if expectedSHA256 != "" && !strings.EqualFold(actualSHA256, expectedSHA256) {
		return "", fmt.Errorf("SHA-256 mismatch: got %s", actualSHA256)
	}
	return actualSHA256, nil
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
