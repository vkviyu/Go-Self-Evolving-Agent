package observation

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var traceName = regexp.MustCompile(`^trace-[0-9]{8}T[0-9]{6}Z-[A-Z2-7]{26,}\.jsonl$`)

// Expire only our finished invocations. Another GSEA process may still own
// an old, idle trace. A missing footer can also be evidence of a host crash.
func expireFinished(dir string, olderThan time.Time) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range files {
		if !entry.Type().IsRegular() || !traceName.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(olderThan) {
			continue
		}
		name := filepath.Join(dir, entry.Name())
		if !hasFooter(name) {
			continue
		}
		// Remove the previous segment before its identifying footer.
		if err := os.Remove(name + ".1"); err != nil && !os.IsNotExist(err) {
			continue
		}
		_ = os.Remove(name)
	}
}

func hasFooter(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil || end == 0 {
		return false
	}
	var tail [512]byte
	part := tail[:min(end, int64(len(tail)))]
	if _, err = f.ReadAt(part, end-int64(len(part))); err != nil || part[len(part)-1] != '\n' {
		return false
	}
	part = part[:len(part)-1]
	part = part[bytes.LastIndexByte(part, '\n')+1:]
	var event Event
	return json.Unmarshal(part, &event) == nil && event.Version == 1 && event.Component == "observer" && event.Name == "recorder.closed" && event.Sequence > 0
}
