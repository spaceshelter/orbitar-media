package video

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestDumpPlans prints the plan for every file in $PLAN_CORPUS as JSON lines, for diffing
// against PictShare's planNormalization on the same files.
func TestDumpPlans(t *testing.T) {
	dir := os.Getenv("PLAN_CORPUS")
	if dir == "" {
		t.Skip("PLAN_CORPUS not set")
	}
	tl := tools(t)
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	sort.Strings(files)
	for _, f := range files {
		p := tl.Probe(context.Background(), f)
		pl := tl.Plan(context.Background(), f, p)
		out := map[string]any{"file": filepath.Base(f), "proper": p.IsProperMP4()}
		if pl != nil {
			out["video"], out["audio"], out["remux"], out["needed"], out["reasons"] = pl.Video, pl.Audio, pl.Remux, pl.Needed, pl.Reasons
			out["args"] = fmt.Sprint(pl.Args("IN", "OUT"))
		}
		if p != nil {
			if w, h, ok := p.Dimensions(); ok {
				out["dims"] = fmt.Sprintf("%dx%d", w, h)
			}
		}
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
	}
}
