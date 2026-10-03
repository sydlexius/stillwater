package rule

import (
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/logging"
	"github.com/sydlexius/stillwater/internal/logging/logtest"
)

// TestPipelineSubComponentsDoNotStackComponentKey drives the real construction
// path behind #2787: NewPipeline tags its logger "fix-pipeline" and then builds
// an EvaluationContext, which tags its own. Handing the tagged logger down
// stacked a second "component" key onto every evalctx line.
func TestPipelineSubComponentsDoNotStackComponentKey(t *testing.T) {
	logger, buf := logtest.NewJSONLogger()
	p := NewPipeline(nil, nil, nil, nil, nil, logger)
	p.SetOrchestrator(&countingEvalProvider{})

	ctx, _ := p.withEvalContext(context.Background(), &artist.Artist{ID: "a1", Name: "A"})
	ec := EvaluationContextFromContext(ctx)
	if ec == nil {
		t.Fatal("withEvalContext installed no EvaluationContext; the test would prove nothing")
	}
	ec.logger.Info("evalctx probe")
	pc := NewPassContext(DefaultPassCacheSize, p.subLogger())
	pc.logger.Info("passctx probe")
	p.logger.Info("pipeline probe")

	out := buf.String()
	if d := logtest.DuplicateKeys(out); len(d) != 0 {
		t.Fatalf("duplicate keys on the real pipeline path: %v", d)
	}
	for _, want := range []string{`"component":"rule.evalctx"`, `"component":"rule.passctx"`, `"component":"fix-pipeline"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(out, `"`+logging.PreviousComponentKey+`"`) {
		t.Fatalf("pipeline handed a tagged logger to a sub-component: %s", out)
	}
}
