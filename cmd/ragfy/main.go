package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ragfy/internal/chunking"
	"ragfy/internal/digest"
	"ragfy/internal/extraction"
	"ragfy/internal/hook"
	"ragfy/internal/model"
	"ragfy/internal/platform"
	"ragfy/internal/retrieval"
	"ragfy/internal/service"
	"ragfy/internal/settings"
	"ragfy/internal/storage/sqlite"
)

var errUnknownCommand = errors.New("unknown command")

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		action := ""
		source := ""
		if len(os.Args) > 2 {
			action = os.Args[2]
		}
		if len(os.Args) > 3 {
			source = os.Args[3]
		}
		dataDirectory, _ := platform.AppDataDir()
		hook.Run(action, source, dataDirectory, settings.ListenAddress, os.Stdin, os.Stdout)
		return
	}
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if err := run(command, os.Args); err != nil {
		if errors.Is(err, errUnknownCommand) {
			os.Exit(2)
		}
		log.Fatal(err)
	}
}

func run(command string, arguments []string) error {
	switch command {
	case "serve":
		return serve()
	case "install":
		return install()
	case "uninstall":
		return platform.UninstallService()
	case "status":
		return platform.Status(settings.ListenAddress, os.Stdout)
	case "setup-hooks":
		return setupHooks()
	case "setup":
		if err := install(); err != nil {
			return err
		}
		return setupHooks()
	case "ingest":
		if len(arguments) < 3 {
			return errors.New("usage: ragfy ingest <folder>")
		}
		return platform.RequestIngest(arguments[2], settings.ListenAddress, os.Stdout)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("%w: %s", errUnknownCommand, command)
	}
}

func serve() error {
	dataDirectory, err := prepareDataDirectory()
	if err != nil {
		return err
	}
	logFile, err := openServiceLog(dataDirectory)
	if err != nil {
		return err
	}
	defer logFile.Close()
	log.SetOutput(logFile)
	token, err := platform.LoadToken(filepath.Join(dataDirectory, "token"))
	if err != nil {
		return err
	}
	repository, err := sqlite.Open(sqlite.Config{Path: filepath.Join(dataDirectory, "memory.sqlite")})
	if err != nil {
		return err
	}
	defer repository.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	log.Println("initializing local embedding model; missing model files will be downloaded")
	embedder, err := model.NewEmbeddingLlama(ctx, filepath.Join(dataDirectory, "models"))
	if err != nil {
		log.Printf("startup failed: %v", err)
		return err
	}
	defer embedder.Close()
	log.Println("startup: validating embedding model and stored vectors")
	if err := service.RefreshEmbeddings(ctx, repository, embedder, model.EmbeddingVersion); err != nil {
		log.Printf("startup failed: %v", err)
		return err
	}
	agentDirectory := filepath.Join(dataDirectory, "agent-exec")
	if err := os.MkdirAll(agentDirectory, 0700); err != nil {
		return err
	}
	schemaPath := filepath.Join(agentDirectory, "digest.schema.json")
	if err := os.WriteFile(schemaPath, []byte(digest.JSONSchema+"\n"), 0600); err != nil {
		return err
	}
	codexExecutable, err := findAgentExecutable("codex")
	if err != nil {
		return err
	}
	claudeExecutable, err := findAgentExecutable("claude")
	if err != nil {
		return err
	}
	extractor := extraction.NewRouter(map[string]extraction.Extractor{
		"codex":  extraction.NewGenerative(model.NewCodexGenerator(codexExecutable, agentDirectory, schemaPath)),
		"claude": extraction.NewGenerative(model.NewClaudeGenerator(claudeExecutable, agentDirectory, digest.JSONSchema)),
	})
	applicationService := service.New(service.Dependencies{
		Repository: repository,
		Embedder:   embedder,
		Extractor:  extractor,
		Retriever:  retrieval.NewHybrid(repository),
		Chunker:    chunking.NewText(1200, 160),
	})
	applicationService.StartWorker(ctx)
	server := &http.Server{Addr: settings.ListenAddress, Handler: applicationService.Routes(token), ReadHeaderTimeout: 3 * time.Second}
	go shutdownOnContext(ctx, server)
	listener, err := net.Listen("tcp", settings.ListenAddress)
	if err != nil {
		log.Printf("startup failed: %v", err)
		return err
	}
	log.Printf("startup: ready; %s active at http://%s", settings.ServiceName, settings.ListenAddress)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func findAgentExecutable(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err == nil {
		candidate := filepath.Join(home, ".local", "bin", name)
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return candidate, nil
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s executable not found: %w", name, err)
	}
	return path, nil
}

func prepareDataDirectory() (string, error) {
	dataDirectory, err := platform.AppDataDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dataDirectory, 0700); err != nil {
		return "", err
	}
	return dataDirectory, nil
}

func openServiceLog(dataDirectory string) (*os.File, error) {
	logDirectory := filepath.Join(dataDirectory, "logs")
	if err := os.MkdirAll(logDirectory, 0700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(logDirectory, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
}

func shutdownOnContext(ctx context.Context, server *http.Server) {
	<-ctx.Done()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownContext)
}

func install() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return platform.InstallService(executable)
}

func setupHooks() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return hook.Setup(executable)
}

func usage() {
	fmt.Println("ragfy {setup|serve|install|uninstall|status|setup-hooks|ingest <folder>|hook [recall|digest] [codex|claude]}")
}
