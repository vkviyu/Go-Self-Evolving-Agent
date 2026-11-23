# Local development pair: go.mod intentionally replaces the unpublished ATE
# module with the sibling checkout. This is not a standalone release build.
.DEFAULT_GOAL := build

.PHONY: build test resilience

build:
	mkdir -p "$(CURDIR)/bin"
	go build -o "$(CURDIR)/bin/ate" github.com/vkviyu/agent-temporal-engine/cmd/ate
	go build -o "$(CURDIR)/bin/gsea" ./cmd/gsea

test:
	go test ./...

resilience:
	go test ./internal/cli -run '^TestResponsesStreamDiagnosticsAndResumeThroughCLI$$' -count=1 -v
	go test ./internal/app -run 'TestTruncatedResponsesStreamPausesAndResumesOriginalTurn|TestTruncatedResponsesAfterConfirmedToolResumesWithoutRepeatingTool' -count=1 -v
	go test ./internal/agentruntime -run 'TestJournalRecoveryPreservesModelVisibleToolFailures|TestJournalRecoveryReplaysEinoDecisionsWithoutRepeatingPreparation|TestCheckpointWithoutCommittedReplayMarkerUsesOriginalJournal' -count=1 -v
