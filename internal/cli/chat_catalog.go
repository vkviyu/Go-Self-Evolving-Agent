package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/sessionregistry"
)

type chatSession struct {
	sessionregistry.Session
	registered bool
}

func canonicalDirectory(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if physical, err := filepath.EvalSymlinks(abs); err == nil {
		return physical
	}
	return abs
}

func workspaceForChat(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return canonicalDirectory(path), nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return canonicalDirectory(wd), nil
}

func chatDataDirectory(resolved config.Config, options chatOptions) string {
	selected := canonicalDirectory(options.dataDirectory)
	if selected == "" {
		selected = canonicalDirectory(resolved.HomeDirectory)
	}
	return selected
}

func findChatSession(ctx context.Context, resolved config.Config, options chatOptions) (chatSession, error) {
	entries, histories, err := app.ReadChatCatalog(ctx, resolved, options.sessionID, options.requireExistingSession)
	if err != nil {
		return chatSession{}, err
	}
	if len(entries) != 0 {
		item := entries[0]
		if strings.TrimSpace(item.DataDirectory) == "" {
			return chatSession{}, fmt.Errorf("session %q has no registered data directory", item.ID)
		}
		if options.dataExplicit && canonicalDirectory(item.DataDirectory) != canonicalDirectory(options.dataDirectory) {
			return chatSession{}, fmt.Errorf("session %q uses data directory %s", item.ID, item.DataDirectory)
		}
		if options.requireExistingSession && len(histories) == 0 {
			return chatSession{}, fmt.Errorf("session %q has no saved conversation", item.ID)
		}
		return chatSession{Session: item, registered: true}, nil
	}
	if len(histories) == 0 {
		return chatSession{}, nil
	}
	return chatSession{Session: historySession(chatDataDirectory(resolved, options), histories[0])}, nil
}

func historySession(dir string, history session.ChatHistory) sessionregistry.Session {
	return sessionregistry.Session{
		ID: string(history.ID), Name: history.Prompt, Workspace: history.Workspace,
		DataDirectory: dir, State: sessionregistry.StateStopped,
		CreatedAt: history.CreatedAt, UpdatedAt: history.UpdatedAt,
	}
}

func applyChatSession(options *chatOptions, item chatSession) string {
	if item.ID == "" {
		return ""
	}
	options.sessionID = session.ID(item.ID)
	if item.DataDirectory != "" {
		options.dataDirectory = item.DataDirectory
	}
	if !options.workspaceExplicit && item.Workspace != "" {
		options.workspaceDirectory = item.Workspace
	}
	if !options.modelExplicit && strings.TrimSpace(item.Model) != "" {
		options.model.Model = item.Model
	}
	if !options.providerExplicit && strings.TrimSpace(item.Provider) != "" {
		options.model.Provider = modelgateway.Provider(item.Provider)
	}
	if item.Name == item.ID {
		return ""
	}
	return strings.TrimSpace(item.Name)
}

// The runtime database lock is held when this is called. ATE catalog records
// retain the runtime resource mapping while history is independently native.
func trackChatSession(ctx context.Context, application *app.App, resolved config.Config, options chatOptions, item chatSession, workspace string) (func() error, error) {
	registry, err := application.SessionRegistry()
	if err != nil {
		return nil, err
	}
	id := string(options.sessionID)
	if !item.registered {
		runtimeDir := filepath.Join(resolved.HomeDirectory, "sessions", id)
		err = registry.Create(ctx, sessionregistry.Session{
			ID: id, Name: id, Workspace: canonicalDirectory(workspace), Runtime: runtimeDir,
			DataDirectory: chatDataDirectory(resolved, options), TempDirectory: filepath.Join(runtimeDir, "tmp"),
			ToolState: filepath.Join(runtimeDir, "tool-state"), Model: options.model.Model, Provider: string(options.model.Provider),
			CreatedAt: item.CreatedAt, UpdatedAt: time.Now(),
		})
		if err != nil {
			_ = registry.Close()
			return nil, err
		}
	}
	if err := registry.UpdateState(ctx, id, sessionregistry.StateRunning, os.Getpid()); err != nil {
		_ = registry.Close()
		return nil, err
	}
	return func() error {
		var errs []error
		if err := registry.UpdateState(context.Background(), id, sessionregistry.StateStopped, 0); err != nil {
			errs = append(errs, fmt.Errorf("mark chat session stopped: %w", err))
		}
		if err := registry.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close chat catalog: %w", err))
		}
		return errors.Join(errs...)
	}, nil
}

// trackChatSessionAsync keeps the interactive startup path independent from
// the latency of an ATE catalog write. The returned cleanup waits for the
// registration attempt before closing the shared authority.
func trackChatSessionAsync(ctx context.Context, application *app.App, resolved config.Config, options chatOptions, item chatSession, workspace string) (func() error, error) {
	type result struct {
		stop func() error
		err  error
	}
	ready := make(chan result, 1)
	go func() {
		stop, err := trackChatSession(ctx, application, resolved, options, item, workspace)
		ready <- result{stop: stop, err: err}
	}()

	var once sync.Once
	var cleanupErr error
	return func() error {
		once.Do(func() {
			outcome := <-ready
			if outcome.err != nil {
				cleanupErr = outcome.err
				return
			}
			if outcome.stop != nil {
				cleanupErr = outcome.stop()
			}
		})
		return cleanupErr
	}, nil
}
