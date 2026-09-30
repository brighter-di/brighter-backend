package strategy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// These payloads are printed and serialized by Studio's actual parser. The
// same fixtures run in the frontend; do not hand-edit the backend copy.
func TestStudioLanguageContract(t *testing.T) {
	data, err := os.ReadFile("testdata/studio-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name     string         `json:"name"`
		Valid    bool           `json:"valid"`
		Strategy CustomStrategy `json:"strategy"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("empty Studio contract")
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			_, err := CompileCustom(fixture.Strategy)
			if (err == nil) != fixture.Valid {
				t.Fatalf("valid=%v, compile error=%v", fixture.Valid, err)
			}
		})
	}
}

func TestGeneratedStudioLanguage(t *testing.T) {
	data, err := os.ReadFile("testdata/studio-language.json")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != studioLanguageSHA {
		t.Fatal("Studio specification and generated compiler definitions are out of sync")
	}
	var spec struct {
		Metrics         []struct{ Name, Windows, BacktestEngine string }
		TransportFields []struct{ Name, GoType string }
		Limits          map[string]int
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	if len(measures) != len(spec.Metrics) {
		t.Fatal("metric count differs from Studio specification")
	}
	for _, metric := range spec.Metrics {
		got, ok := measures[metric.Name]
		if !ok || got.fn != metric.BacktestEngine || got.windowed != (metric.Windows == "lookback") || got.oscillator != (metric.Windows == "oscillator" || metric.Windows == "signal") || got.smooth != (metric.Windows == "signal") {
			t.Errorf("metric %s differs from Studio specification", metric.Name)
		}
	}
	fields := make(map[string]string)
	typeOf := reflect.TypeFor[CustomNode]()
	for i := range typeOf.NumField() {
		field := typeOf.Field(i)
		name := field.Tag.Get("json")
		for j, char := range name {
			if char == ',' {
				name = name[:j]
				break
			}
		}
		typeName := field.Type.String()
		if typeName == "[]strategy.CustomNode" {
			typeName = "[]CustomNode"
		}
		if typeName == "*strategy.CustomNode" {
			typeName = "*CustomNode"
		}
		fields[name] = typeName
	}
	for _, field := range spec.TransportFields {
		if fields[field.Name] != field.GoType {
			t.Errorf("transport field %s differs from Studio specification", field.Name)
		}
	}
	limits := map[string]int{"maxDepth": customMaxDepth, "maxBlocks": customMaxBlocks, "maxList": customMaxList, "maxCount": customMaxCount, "minLookback": customMinDays, "maxLookback": customMaxDays, "maxName": customMaxName, "maxValue": customMaxValue, "minCorridor": customMinCorridor, "maxCorridor": customMaxCorridor, "maxConditionParts": customMaxConditionParts, "maxExpandedBlocks": customMaxExpandedBlocks, "minPercent": customMinPercent, "maxPercent": customMaxPercent}
	for name, got := range limits {
		if got != spec.Limits[name] {
			t.Errorf("limit %s differs from Studio specification", name)
		}
	}
}
