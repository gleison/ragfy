package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ragfy/internal/buildinfo"
	"ragfy/internal/chunking"
	appconfig "ragfy/internal/config"
	"ragfy/internal/digest"
	"ragfy/internal/extraction"
	"ragfy/internal/hook"
	"ragfy/internal/model"
	"ragfy/internal/platform"
	"ragfy/internal/reconciliation"
	"ragfy/internal/retrieval"
	"ragfy/internal/service"
	"ragfy/internal/settings"
	setupflow "ragfy/internal/setup"
	statusview "ragfy/internal/status"
	"ragfy/internal/storage/sqlite"
	"ragfy/internal/updater"
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
	command := "status"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if err := run(command, os.Args); err != nil {
		if errors.Is(err, errUnknownCommand) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "ragfy:", err)
		os.Exit(1)
	}
}

func run(command string, arguments []string) error {
	switch command {
	case "serve":
		return serve()
	case "install":
		return install()
	case "uninstall":
		return setupflow.Uninstall()
	case "status":
		return statusCommand(commandArguments(arguments), os.Stdout)
	case "setup-hooks":
		return setupHooks()
	case "setup":
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		return setupflow.Run(context.Background(), executable, os.Stdin, os.Stdout)
	case "ingest":
		if err := requireSetup(); err != nil {
			return err
		}
		return ingest(arguments[2:], os.Stdout)
	case "version", "--version":
		fmt.Fprintf(os.Stdout, "ragfy %s (%s, %s)\n", buildinfo.DisplayVersion(), buildinfo.Commit, buildinfo.Date)
		return nil
	case "update":
		return updater.Run(os.Stdin, os.Stdout, os.Stderr)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("%w: %s", errUnknownCommand, command)
	}
}

func commandArguments(arguments []string) []string {
	if len(arguments) <= 2 {
		return nil
	}
	return arguments[2:]
}

func statusCommand(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "write machine-readable status")
	verbose := flags.Bool("verbose", false, "show diagnostic paths and errors")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return errors.New("usage: ragfy status [--json|--verbose]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	report := statusview.Inspect(ctx, buildinfo.DisplayVersion(), settings.ListenAddress)
	return statusview.Write(output, report, *asJSON, *verbose)
}

func ingest(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("ingest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	project := flags.String("project", "", "limit ingested documents to a project")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 1 {
		return errors.New("usage: ragfy ingest [--project <name>] <folder>")
	}
	return platform.RequestIngest(flags.Arg(0), *project, settings.ListenAddress, output)
}

func serve() (resultErr error) {
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
	defer func() {
		if resultErr != nil {
			log.Printf("startup failed: %v", resultErr)
		}
	}()
	configuration, err := appconfig.LoadRequired(filepath.Join(dataDirectory, "config.json"))
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
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
	embedder, err := model.NewEmbeddingLlamaWithConfig(ctx, filepath.Join(dataDirectory, "models"), configuration.Embedding)
	if err != nil {
		log.Printf("startup failed: %v", err)
		return err
	}
	defer embedder.Close()
	log.Println("startup: validating embedding model and stored vectors")
	if err := service.RefreshEmbeddings(ctx, repository, embedder, configuration.Embedding.ModelVersion()); err != nil {
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
	reconciliationSchemaPath := filepath.Join(agentDirectory, "reconciliation.schema.json")
	if err := os.WriteFile(reconciliationSchemaPath, []byte(reconciliation.JSONSchema+"\n"), 0600); err != nil {
		return err
	}
	extractors := map[string]extraction.Extractor{}
	reconcilers := map[string]reconciliation.Reconciler{}
	if configuration.Enabled("codex") {
		codexExecutable, executableErr := findAgentExecutable("codex")
		if executableErr != nil {
			return executableErr
		}
		extractors["codex"] = extraction.NewGenerative(model.NewCodexGenerator(codexExecutable, agentDirectory, schemaPath))
		reconcilers["codex"] = reconciliation.NewGenerative(model.NewCodexGenerator(codexExecutable, agentDirectory, reconciliationSchemaPath))
	}
	if configuration.Enabled("claude") {
		claudeExecutable, executableErr := findAgentExecutable("claude")
		if executableErr != nil {
			return executableErr
		}
		extractors["claude"] = extraction.NewGenerative(model.NewClaudeGenerator(claudeExecutable, agentDirectory, digest.JSONSchema))
		reconcilers["claude"] = reconciliation.NewGenerative(model.NewClaudeGenerator(claudeExecutable, agentDirectory, reconciliation.JSONSchema))
	}
	extractor := extraction.NewRouter(extractors)
	reconciler := reconciliation.NewRouter(reconcilers)
	applicationService := service.New(service.Dependencies{
		Repository: repository,
		Embedder:   embedder,
		Extractor:  extractor,
		Retriever:  retrieval.NewHybrid(repository),
		Reconciler: reconciler,
		Chunker:    chunking.NewText(1200, 160),
		Runtime: service.RuntimeInfo{
			Version: buildinfo.DisplayVersion(),
			Model:   configuration.Embedding.Name(),
			Pooling: configuration.Embedding.Pooling,
		},
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
	return platform.FindExecutable(name)
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
	dataDirectory, err := platform.AppDataDir()
	if err != nil {
		return err
	}
	configuration, err := appconfig.LoadRequired(filepath.Join(dataDirectory, "config.json"))
	if err != nil {
		return err
	}
	return hook.Configure(executable, configuration.Integrations)
}

func requireSetup() error {
	dataDirectory, err := platform.AppDataDir()
	if err != nil {
		return err
	}
	_, err = appconfig.LoadRequired(filepath.Join(dataDirectory, "config.json"))
	return err
}

func usage() {
	fmt.Println("ragfy {setup|status [--verbose|--json]|update|ingest [--project <name>] <folder>|version|uninstall}")
}
