package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestRevisionRecordRejectsInvalidMetadataAndContent(t *testing.T) {
	source := []byte("test source")
	sum := sha256.Sum256(source)
	record := RevisionRecord{Version: CatalogVersion, Manifest: manifest("v1"), ContentSHA256: hex.EncodeToString(sum[:]), RuntimeIdentity: "limits"}
	record.Manifest.Timeout = time.Second
	encoded, err := record.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRecord(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateSource(source); err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateSource([]byte("changed")); err == nil {
		t.Fatal("accepted incorrect digest")
	}
	for _, invalid := range [][]byte{
		append(append([]byte(nil), encoded...), []byte(` {}`)...),
		[]byte(strings.Replace(string(encoded), `"Version":1`, `"Version":999`, 1)),
		[]byte(strings.Replace(string(encoded), `"RuntimeIdentity":`, `"UnknownField":`, 1)),
		[]byte(strings.Repeat(" ", MaxManifestBytes+1)),
	} {
		if _, err := DecodeRecord(invalid); err == nil {
			t.Fatal("accepted invalid manifest")
		}
	}
	for _, change := range []func(*RevisionRecord){
		func(r *RevisionRecord) { r.Manifest.Timeout = 0 },
		func(r *RevisionRecord) { r.Manifest.PluginID = strings.Repeat("p", 129) },
		func(r *RevisionRecord) { r.ContentSHA256 = strings.ToUpper(r.ContentSHA256) },
		func(r *RevisionRecord) { r.Manifest.Tool.Description = strings.Repeat("x", MaxManifestBytes) },
	} {
		invalid := record
		change(&invalid)
		if _, err := invalid.Encode(); err == nil {
			t.Fatal("encoded invalid record")
		}
	}
}
