package validator

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-cmp/cmp"
	"github.com/nikolalohinski/gonja/v2"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
)

type StatementBuilder struct {
	template string
}

func NewStatementBuilder(template string) *StatementBuilder {
	return &StatementBuilder{
		template: template,
	}
}

type pythonNone struct{}

func (pythonNone) String() string {
	return "None"
}

func (pythonNone) MarshalJSON() ([]byte, error) {
	return []byte("null"), nil
}

func isPythonNone(val interface{}) bool {
	_, ok := val.(pythonNone)
	return ok
}

var (
	templateOutputExprPattern = regexp.MustCompile(`\{\{\s*(.*?)\s*\}\}`)
	templateBlockExprPattern  = regexp.MustCompile(`\{%\s*(.*?)\s*%\}`)
	dottedPathPattern         = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+\b`)
	dictGetPathPattern        = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\.get\(\s*["']([A-Za-z_][A-Za-z0-9_]*)["']`)
)

// unguardedConcatPathsCache memoizes, per template string, the dotted paths that
// participate in a "~" concatenation but are never used as an {% if %} guard.
// The template text is static per StatementBuilder, so re-running the regex scan
// on every Build() call (i.e. every signature verification) is wasted work.
var unguardedConcatPathsCache sync.Map // map[string][]string

func prepareDataForPythonJinjaConcat(data map[string]interface{}, template string) map[string]interface{} {
	for _, path := range getUnguardedConcatPaths(template) {
		replaceNilPathWithPythonNone(data, strings.Split(path, "."))
	}

	return data
}

func getUnguardedConcatPaths(template string) []string {
	if cached, ok := unguardedConcatPathsCache.Load(template); ok {
		return cached.([]string)
	}

	guardedPaths := collectGuardedTemplatePaths(template)
	pathSet := make(map[string]struct{})
	for _, match := range templateOutputExprPattern.FindAllStringSubmatch(template, -1) {
		if len(match) < 2 || !strings.Contains(match[1], "~") {
			continue
		}
		for _, path := range collectTemplateExpressionPaths(match[1]) {
			if _, guarded := guardedPaths[path]; !guarded {
				pathSet[path] = struct{}{}
			}
		}
	}

	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		paths = append(paths, path)
	}

	cached, _ := unguardedConcatPathsCache.LoadOrStore(template, paths)
	return cached.([]string)
}

func collectGuardedTemplatePaths(template string) map[string]struct{} {
	paths := make(map[string]struct{})
	for _, match := range templateBlockExprPattern.FindAllStringSubmatch(template, -1) {
		if len(match) < 2 {
			continue
		}
		for _, path := range collectTemplateExpressionPaths(match[1]) {
			paths[path] = struct{}{}
		}
	}
	return paths
}

func collectTemplateExpressionPaths(expression string) []string {
	paths := make(map[string]struct{})
	for _, match := range dictGetPathPattern.FindAllStringSubmatch(expression, -1) {
		if len(match) == 3 {
			paths[match[1]+"."+match[2]] = struct{}{}
		}
	}
	for _, path := range dottedPathPattern.FindAllString(expression, -1) {
		if strings.HasSuffix(path, ".get") {
			continue
		}
		paths[path] = struct{}{}
	}

	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	return result
}

func replaceNilPathWithPythonNone(data map[string]interface{}, path []string) {
	if len(path) == 0 {
		return
	}
	current := data
	for _, key := range path[:len(path)-1] {
		next, ok := current[key].(map[string]interface{})
		if !ok {
			return
		}
		current = next
	}

	last := path[len(path)-1]
	if value, exists := current[last]; exists && value == nil {
		current[last] = pythonNone{}
	}
}

// getGonjaFilters returns a list of custom filters to be used with Gonja
func getGonjaFilters() map[string]exec.FilterFunction {
	return map[string]exec.FilterFunction{
		"toString": func(e *exec.Evaluator, in *exec.Value, params *exec.VarArgs) *exec.Value {
			val := in.Interface()

			// Match Python logic:
			// - If it's an int or float, convert to string first, then JSON marshal
			// - If it's Python None, preserve JSON null for direct toString rendering
			// - Otherwise, directly marshal the value
			switch v := val.(type) {
			case int:
				// For integers, convert to string representation, then JSON marshal
				str := fmt.Sprintf("%v", v)
				bytes, _ := json.Marshal(str)
				return exec.AsValue(string(bytes))
			case pythonNone:
				return exec.AsValue("null")
			case float64:
				// For floats, convert to string representation, then JSON marshal
				str := fmt.Sprintf("%v", v)
				bytes, _ := json.Marshal(str)
				return exec.AsValue(string(bytes))

			default:
				// For all other types (string, bool, nil, arrays), directly marshal to JSON
				bytes, _ := json.Marshal(val)
				return exec.AsValue(string(bytes))
			}
		},
		"toInt": func(e *exec.Evaluator, in *exec.Value, params *exec.VarArgs) *exec.Value {
			switch val := in.Interface().(type) {
			case float64:
				return exec.AsValue(int(val))
			case int:
				return exec.AsValue(val)
			case string:
				var result int
				fmt.Sscanf(val, "%d", &result)
				return exec.AsValue(result)
			default:
				return exec.AsValue(0)
			}
		},
		"len": func(e *exec.Evaluator, in *exec.Value, params *exec.VarArgs) *exec.Value {
			switch val := in.Interface().(type) {
			case []interface{}:
				return exec.AsValue(len(val))
			case map[string]interface{}:
				return exec.AsValue(len(val))
			case string:
				return exec.AsValue(len(val))
			default:
				return exec.AsValue(0)
			}
		},
		"toList1": func(e *exec.Evaluator, in *exec.Value, params *exec.VarArgs) *exec.Value {
			if slice, ok := in.Interface().([]interface{}); ok {
				var result []string
				for _, item := range slice {
					if item != nil && !isPythonNone(item) {
						result = append(result, fmt.Sprintf("%v", item))
					}
				}
				bytes, _ := json.Marshal(result)
				return exec.AsValue(string(bytes))
			}
			return exec.AsValue("[]")
		},
		"toList2": func(e *exec.Evaluator, in *exec.Value, params *exec.VarArgs) *exec.Value {
			if slice, ok := in.Interface().([]interface{}); ok {
				var result [][]string
				for _, row := range slice {
					if rowSlice, ok := row.([]interface{}); ok {
						var rowResult []string
						for _, item := range rowSlice {
							if item != nil && !isPythonNone(item) {
								rowResult = append(rowResult, fmt.Sprintf("%v", item))
							}
						}
						if len(rowResult) > 0 {
							result = append(result, rowResult)
						}
					}
				}
				bytes, _ := json.Marshal(result)
				return exec.AsValue(string(bytes))
			}
			return exec.AsValue("[]")
		},
		"toRules": func(e *exec.Evaluator, in *exec.Value, params *exec.VarArgs) *exec.Value {
			if slice, ok := in.Interface().([]interface{}); ok {
				var result []map[string]string
				for _, item := range slice {
					if mapItem, ok := item.(map[string]interface{}); ok {
						ruleMap := make(map[string]string)
						for k, v := range mapItem {
							if v != nil && !isPythonNone(v) {
								ruleMap[k] = fmt.Sprintf("%v", v)
							}
						}
						if len(ruleMap) > 0 {
							result = append(result, ruleMap)
						}
					}
				}
				bytes, _ := json.Marshal(result)
				return exec.AsValue(string(bytes))
			}
			return exec.AsValue("[]")
		},
	}
}

func getGonjaDictMethods() *exec.MethodSet[map[string]interface{}] {
	return exec.NewMethodSet[map[string]interface{}](map[string]exec.Method[map[string]interface{}]{
		// keys method is builtins method defined in https://github.com/NikolaLohinski/gonja/blob/master/builtins/methods/dict.go#L9
		"keys": func(self map[string]interface{}, selfValue *exec.Value, arguments *exec.VarArgs) (interface{}, error) {
			if err := arguments.Take(); err != nil {
				return nil, exec.ErrInvalidCall(err)
			}
			keys := make([]string, 0)
			for key := range self {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			return keys, nil
		},
		"get": func(self map[string]interface{}, selfValue *exec.Value, arguments *exec.VarArgs) (interface{}, error) {
			if len(arguments.Args) < 1 || len(arguments.Args) > 2 {
				return nil, exec.ErrInvalidCall(fmt.Errorf("get method expects 1 or 2 arguments, got %d", len(arguments.Args)))
			}

			key, ok := arguments.Args[0].Interface().(string)
			if !ok {
				return nil, exec.ErrInvalidCall(fmt.Errorf("get method expects string key"))
			}

			// if key exists, return value
			if value, exists := self[key]; exists {
				return value, nil
			}

			// if key not exists and has default value, return default value
			if len(arguments.Args) == 2 {
				return arguments.Args[1].Interface(), nil
			}

			// if key not exists and no default value, return nil
			return nil, nil
		},
		"items": func(self map[string]interface{}, selfValue *exec.Value, arguments *exec.VarArgs) (interface{}, error) {
			if err := arguments.Take(); err != nil {
				return nil, exec.ErrInvalidCall(err)
			}
			items := make([][]interface{}, 0)
			for key, value := range self {
				items = append(items, []interface{}{key, value})
			}
			// Sort by key for consistent output
			sort.Slice(items, func(i, j int) bool {
				return fmt.Sprintf("%v", items[i][0]) < fmt.Sprintf("%v", items[j][0])
			})
			return items, nil
		},
	})
}

func (s *StatementBuilder) Build(bizData string) (string, error) {
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(bizData), &data); err != nil {
		fmt.Printf("Error parsing JSON data for build statement: %v\n", err)
		return "", fmt.Errorf("error parsing JSON data: %w", err)
	}
	data = prepareDataForPythonJinjaConcat(data, s.template)

	template, err := getGonjaTemplate(s.template)
	if err != nil {
		return "", fmt.Errorf("error parsing template: %w", err)
	}

	// Create context with data
	context := exec.NewContext(data)

	// Render the template
	message, err := template.ExecuteToString(context)
	if err != nil {
		return "", fmt.Errorf("error rendering template: %w", err)
	}

	// Convert JSON to compact string without formatting while preserving key order
	// First validate that it's valid JSON and compact it
	var buf bytes.Buffer
	err = json.Compact(&buf, []byte(message))
	if err != nil {
		return "", fmt.Errorf("compact rendered template failed: %v", err)
	}

	// json.Compact escapes HTML characters like <, >, &
	// We need to unescape them to get the original characters
	result := buf.String()
	result = strings.ReplaceAll(result, `\u003c`, "<")
	result = strings.ReplaceAll(result, `\u003e`, ">")
	result = strings.ReplaceAll(result, `\u0026`, "&")

	return result, nil
}

func getGonjaTemplate(source string) (*exec.Template, error) {
	// Get custom filters
	customFilters := getGonjaFilters()

	// Create a new filter set with custom filters
	customFilterSet := exec.NewFilterSet(customFilters)

	// Merge with default filters using Update method
	filterSet := gonja.DefaultEnvironment.Filters.Update(customFilterSet)

	methods := gonja.DefaultEnvironment.Methods

	// Get custom dict methods to the methods
	methods.Dict = getGonjaDictMethods()

	// Create environment with merged filters and methods
	env := &exec.Environment{
		Context:           gonja.DefaultEnvironment.Context,
		Filters:           filterSet,
		ControlStructures: gonja.DefaultEnvironment.ControlStructures,
		Tests:             gonja.DefaultEnvironment.Tests,
		Methods:           methods,
	}

	sourceBytes := []byte(source)
	rootID := fmt.Sprintf("root-%s", string(sha256.New().Sum(sourceBytes)))

	loader, err := loaders.NewFileSystemLoader("")
	if err != nil {
		return nil, err
	}
	shiftedLoader, err := loaders.NewShiftedLoader(rootID, bytes.NewReader(sourceBytes), loader)
	if err != nil {
		return nil, err
	}

	return exec.NewTemplate(rootID, gonja.DefaultConfig, shiftedLoader, env)
}

func CompareStatementMessage(message1, message2 string) (bool, string) {
	var data1, data2 interface{}

	// Parse first JSON string
	if err := json.Unmarshal([]byte(message1), &data1); err != nil {
		return false, fmt.Sprintf("failed to parse first statement message: %v", err)
	}

	// Parse second JSON string
	if err := json.Unmarshal([]byte(message2), &data2); err != nil {
		return false, fmt.Sprintf("failed to parse second statement message: %v", err)
	}

	if diff := cmp.Diff(data1, data2); diff != "" {
		return false, fmt.Sprintf("statement message differences:\n%s", diff)
	}

	return true, ""
}
