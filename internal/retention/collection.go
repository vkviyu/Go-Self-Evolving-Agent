package retention

import (
	"encoding/json"
	"time"
)

// CollectionPlan is scoped to one exact revision, never a directory/glob.
// Unreferenced manifests and metadata remain as audit evidence after collection.
type CollectionPlan struct {
	ID               string   `json:"plan_id"`
	Tool             string   `json:"tool"`
	Revision         string   `json:"revision"`
	ContentSHA256    string   `json:"content_sha256"`
	State            string   `json:"state"`
	ArtifactBytes    int      `json:"artifact_bytes"`
	ReclaimableBytes int      `json:"reclaimable_blob_bytes"`
	Blockers         []string `json:"blockers"`
}

func (plan CollectionPlan) Digest() string {
	plan.ID = ""
	encoded, _ := json.Marshal(plan)
	return Digest(encoded)
}

type CollectionReceipt struct {
	Plan        CollectionPlan `json:"plan"`
	CollectedAt time.Time      `json:"collected_at"`
	BlobDeleted bool           `json:"blob_deleted"`
}
