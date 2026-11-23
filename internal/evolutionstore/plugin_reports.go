package evolutionstore

import (
	"context"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
)

func (s *Store) SavePluginTest(ctx context.Context, report plugin.TestReport) (string, error) {
	if report.Tool == "" || report.Revision == "" || len(report.Results) == 0 {
		return "", errors.New("invalid plugin test report")
	}
	doc, err := encode(report)
	if err != nil {
		return "", err
	}
	err = s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		return saveDocument(tx, "plugin-test-report", doc)
	})
	if err != nil {
		return "", err
	}
	return doc.Digest, nil
}

func (s *Store) PluginTest(ctx context.Context, id string) (report plugin.TestReport, err error) {
	err = s.authority.View(ctx, func(tx *ateauthority.Tx) error {
		return loadDocument(tx, "plugin-test-report", id, &report)
	})
	return
}
