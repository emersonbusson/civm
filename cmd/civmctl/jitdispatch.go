package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/emersonbusson/civm/internal/jitdispatcher"
)

const (
	exitJITFailure   = 1
	exitJITAmbiguous = 2
	exitJITBusy      = 75
)

func runJITDispatch(args []string) int {
	return runJITDispatchWithIO(args, os.Stdin, os.Stdout, os.Stderr)
}

func runJITDispatchWithIO(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("jit-dispatch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "config host-local absoluta")
	repository := flags.String("repo", "", "owner/repo allowlisted")
	candidateRef := flags.String("candidate-ref", "", "ref exata refs/heads/...")
	candidateSHA := flags.String("candidate-sha", "", "SHA exato de 40 ou 64 hex")
	idempotency := flags.String("idempotency-key", "", "chave opaca de replay")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return exitUsage
	}
	if *configPath == "" || *repository == "" || *candidateRef == "" || *candidateSHA == "" || *idempotency == "" {
		fmt.Fprintln(stderr, "jit-dispatch: todas as flags são obrigatórias; o token entra somente por stdin")
		return exitUsage
	}
	config, err := jitdispatcher.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "jit-dispatch: config recusada: %v\n", err)
		return exitUsage
	}
	request := jitdispatcher.Request{
		Repository: *repository, CandidateRef: *candidateRef,
		CandidateSHA: *candidateSHA, Idempotency: *idempotency,
	}
	policy, ok := config.Policy(request.Repository)
	if !ok {
		fmt.Fprintln(stderr, "jit-dispatch: repositório fora da allowlist")
		return exitUsage
	}
	if err := jitdispatcher.ValidateRequest(request, policy); err != nil {
		fmt.Fprintf(stderr, "jit-dispatch: request recusado: %v\n", err)
		return exitUsage
	}
	store, err := jitdispatcher.OpenStore(config.StateDir)
	if err != nil {
		fmt.Fprintf(stderr, "jit-dispatch: state recusado: %v\n", err)
		return exitUsage
	}
	token, err := jitdispatcher.ReadToken(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "jit-dispatch: token recusado: %v\n", err)
		return exitUsage
	}
	defer jitdispatcher.Zero(token)
	httpClient := &http.Client{Timeout: config.HTTPTimeout}
	github, err := jitdispatcher.NewGitHubClient(config.APIBaseURL, config.APIVersion, token, httpClient)
	if err != nil {
		fmt.Fprintf(stderr, "jit-dispatch: cliente recusado: %v\n", err)
		return exitUsage
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	selfExecutable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "jit-dispatch: executável atual indisponível: %v\n", err)
		return exitJITFailure
	}
	dispatcher, err := jitdispatcher.NewDispatcher(config, jitdispatcher.Dependencies{
		GitHub: github, Store: store, Runner: jitdispatcher.NewExecRunner(),
		Gate:   jitdispatcher.NewExecResourceGate(selfExecutable),
		Logger: logger, Sensitive: [][]byte{token},
	})
	if err != nil {
		fmt.Fprintf(stderr, "jit-dispatch: inicialização recusada: %v\n", err)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, dispatchErr := dispatcher.Dispatch(ctx, request)
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		fmt.Fprintf(stderr, "jit-dispatch: saída JSON falhou: %v\n", err)
		return exitJITFailure
	}
	if dispatchErr == nil {
		return 0
	}
	fmt.Fprintf(stderr, "jit-dispatch: %v\n", dispatchErr)
	return jitDispatchExit(dispatchErr)
}

func runJITLeaseHold(args []string) int {
	flags := flag.NewFlagSet("__jit-lease-hold", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	leaseID := flags.String("lease-id", "", "")
	admissionID := flags.String("admission-id", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *leaseID == "" || *admissionID == "" {
		return exitUsage
	}
	if err := jitdispatcher.RunLeaseHolder(os.Stdin, *leaseID, *admissionID, jitdispatcher.LeaseMarkerPath); err != nil {
		return exitJITFailure
	}
	return 0
}

func jitDispatchExit(err error) int {
	switch {
	case errors.Is(err, jitdispatcher.ErrInvalid):
		return exitUsage
	case errors.Is(err, jitdispatcher.ErrBusy):
		return exitJITBusy
	case errors.Is(err, jitdispatcher.ErrAmbiguous),
		errors.Is(err, jitdispatcher.ErrStale),
		errors.Is(err, jitdispatcher.ErrReplay):
		return exitJITAmbiguous
	default:
		return exitJITFailure
	}
}
