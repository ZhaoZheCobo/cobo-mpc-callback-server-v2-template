package validator

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// knownKeyOrderDivergence and knownContentDivergence are the counts of two
// understood, NOT-yet-fixed Python/Go rendering divergences, as of the last time
// this matrix was regenerated and reviewed by hand. They exist so this test can
// fail on a *new* divergence class without requiring an unmaintainable per-case
// allowlist for issues that are already tracked. If either count changes, that is
// itself a signal worth investigating (fewer is a regression fix worth confirming
// deliberately; more means a new template or a gen_matrix.py change introduced a
// new instance of an old class - or a genuinely new class, which won't show up
// under these buckets and WILL fail the test below).
//
// 1. Key-order divergence (currently the large bucket): Go's encoding/json always
// sorts map keys when marshaling (documented stdlib behavior); Python's json.dumps
// preserves original insertion order. Any template field that dumps a whole nested
// object via "| toString" (e.g. destination.calldata_info) reproduces Python's
// object key order only by coincidence of how far upstream produced it - nothing
// enforces it. This is SECURITY-RELEVANT, not cosmetic: the SDK validator hashes
// the raw rebuilt message bytes to check the signature, so a reordered-but-
// equivalent JSON object is a byte-different, verification-failing message. Fixing
// this needs the biz_data JSON parse to preserve object key order end-to-end
// (map[string]interface{} discards it; gonja's dict access would need an
// order-preserving map type threaded through Build()) - a bigger, separate change,
// not a contained patch. Detected here via CompareStatementMessage (semantic JSON
// equality) being true despite the raw strings differing.
//
// 2. Content divergence (numeric formatting + non-list for-loop targets): Go's
// encoding/json decodes every JSON number into float64, losing whether the
// original literal was an int or a float; gonja's Value.String() for a float64
// always appends ".0" to whole numbers, so a bare numeric field participating in a
// "~" concatenation renders as e.g. "0.0" instead of Python's "0". Relatedly,
// gonja's {% for %} silently no-ops over a non-list value where Python raises
// TypeError. Neither is reachable with real biz_data today: the fields that hit
// this are always JSON strings in production (chain_id, token_id, ...), and a
// for-loop target is always a real list or legitimately absent - this only fires
// when a field is fuzzed into a type it would never actually hold. Not fixed here
// because the correct fix (decode biz_data with json.Decoder.UseNumber()) can't be
// contained: gonja's IsTrue() dispatches on reflect.Kind with no interface-based
// override hook (same constraint as pythonNone in statement.go), and a
// json.Number's Kind() is String, so {% if amount %} would start treating "0" as
// truthy - fixing it correctly means forking gonja's Value type.
const (
	knownKeyOrderDivergence = 1376
	knownContentDivergence  = 129
	knownGoTooLenientErrors = 4 // python raises, go silently succeeds - same for-loop/type-coercion leniency as bucket 2
)

// TestConformanceMatrix diffs Go's rendering against real cobo-libs (Python/Jinja2)
// output across a generated edge-case matrix (null/empty/zero/negative/huge-int/
// unicode mutations of every scalar field in every template's example data).
//
// This is not a self-contained unit test: it consumes a fixture file produced by
// conformance/gen_matrix.py, which must be run against a local cobo-libs checkout
// (see that file's docstring). Regenerate with:
//
//	COBO_LIBS_ROOT=/path/to/cobo-libs \
//	EXAMPLE_DATAS_DIR=$(pwd)/template_datas/example_datas \
//	OUT_FILE=$(pwd)/conformance/cases.jsonl \
//	    /path/to/cobo-libs/.venv/bin/python3 conformance/gen_matrix.py
//
// The point is to catch the next Python/Go rendering divergence (there will be one -
// see statement.go's history) at template-authoring/CI time instead of on a real,
// possibly mobile-signed, transaction. Skips (does not fail) if the fixture or the
// cobo-libs template source isn't available locally, since both require an external
// cobo-libs checkout this repo doesn't own.
func TestConformanceMatrix(t *testing.T) {
	casesPath := filepath.Join("conformance", "cases.jsonl")
	templatesDirEnv := os.Getenv("COBO_LIBS_ROOT")

	f, err := os.Open(casesPath)
	if err != nil {
		t.Skipf("no conformance fixture at %s (run conformance/gen_matrix.py first): %v", casesPath, err)
	}
	defer f.Close()

	if templatesDirEnv == "" {
		t.Skip("COBO_LIBS_ROOT not set; cannot resolve the templates referenced by the fixture")
	}
	templatesDir := filepath.Join(templatesDirEnv, "cobo_libs/cobo_auth/templates/json_templates")

	type conformanceCase struct {
		TemplateFile  string          `json:"template_file"`
		Path          string          `json:"path"`
		EdgeLabel     string          `json:"edge_label"`
		BizData       json.RawMessage `json:"biz_data"`
		PythonMessage *string         `json:"python_message"`
		PythonError   *string         `json:"python_error"`
	}

	templateCache := map[string]string{}
	total, bothError, exactMatch := 0, 0, 0
	keyOrderOnly, contentDivergence, pyErrGoOk, pyOkGoErr := 0, 0, 0, 0

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c conformanceCase
		if err := json.Unmarshal(line, &c); err != nil {
			t.Fatalf("bad fixture line: %v", err)
		}
		total++

		content, ok := templateCache[c.TemplateFile]
		if !ok {
			b, err := os.ReadFile(filepath.Join(templatesDir, c.TemplateFile))
			if err != nil {
				t.Fatalf("read template %s: %v", c.TemplateFile, err)
			}
			content = string(b)
			templateCache[c.TemplateFile] = content
		}

		goMsg, goErr := NewStatementBuilder(content).Build(string(c.BizData))
		pyHasErr, goHasErr := c.PythonError != nil, goErr != nil

		switch {
		case pyHasErr && goHasErr:
			bothError++
		case !pyHasErr && !goHasErr:
			if goMsg == *c.PythonMessage {
				exactMatch++
				continue
			}
			if eq, _ := CompareStatementMessage(*c.PythonMessage, goMsg); eq {
				keyOrderOnly++
				continue
			}
			contentDivergence++
			t.Logf("content divergence: template=%s path=%s edge=%s\n  py: %s\n  go: %s",
				c.TemplateFile, c.Path, c.EdgeLabel, *c.PythonMessage, goMsg)
		case pyHasErr && !goHasErr:
			pyErrGoOk++
			t.Logf("python errors but go succeeds: template=%s path=%s edge=%s\n  py_err: %s\n  go_msg: %s",
				c.TemplateFile, c.Path, c.EdgeLabel, *c.PythonError, goMsg)
		default:
			pyOkGoErr++
			t.Errorf("NEW: go errors but python succeeds: template=%s path=%s edge=%s\n  py_msg: %s\n  go_err: %s",
				c.TemplateFile, c.Path, c.EdgeLabel, *c.PythonMessage, goErr)
		}
	}

	t.Logf("total=%d exactMatch=%d bothError=%d keyOrderOnly=%d(known=%d) contentDivergence=%d(known=%d) pyErrGoOk=%d(known=%d) pyOkGoErr=%d",
		total, exactMatch, bothError, keyOrderOnly, knownKeyOrderDivergence,
		contentDivergence, knownContentDivergence, pyErrGoOk, knownGoTooLenientErrors, pyOkGoErr)

	if pyOkGoErr > 0 {
		t.Errorf("Go is stricter than Python in %d case(s) - Go should never reject data Python accepts", pyOkGoErr)
	}
	if contentDivergence > knownContentDivergence {
		t.Errorf("content divergence grew from known baseline %d to %d - new Python/Go rendering gap introduced",
			knownContentDivergence, contentDivergence)
	}
	if pyErrGoOk > knownGoTooLenientErrors {
		t.Errorf("go-too-lenient-vs-python-errors grew from known baseline %d to %d", knownGoTooLenientErrors, pyErrGoOk)
	}
	// keyOrderOnly is intentionally not gated on growth: it's already large and
	// pervasive (see const doc above) and gating it would just train reviewers to
	// bump the constant without reading it. It's logged every run so it stays
	// visible until the underlying order-preserving-parse fix lands.
}
