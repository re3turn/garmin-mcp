package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, text string) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func shapeJSON(t *testing.T, text string) string {
	t.Helper()
	data, err := json.Marshal(shape(decode(t, text)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestShapeKeepsNoValues(t *testing.T) {
	got := shapeJSON(t, `{"displayName":"runner42","fullName":"Jane Doe","profileId":123456789,
		"vo2Max":52.5,"isPro":false,"photo":null}`)
	for _, leaked := range []string{"runner42", "Jane Doe", "123456789", "52.5"} {
		if strings.Contains(got, leaked) {
			t.Errorf("shape holds %q: %s", leaked, got)
		}
	}
	want := `{"displayName":"string","fullName":"string","isPro":"boolean","photo":"null","profileId":"number","vo2Max":"number"}`
	if got != want {
		t.Errorf("shape = %s, want %s", got, want)
	}
}

func TestShapeMergesArrayElements(t *testing.T) {
	got := shapeJSON(t, `[{"a":1,"b":"x"},{"a":null,"c":true},{"b":{"d":2}}]`)
	want := `[{"a":"null|number","b":{"d":"number"},"c":"boolean"}]`
	if got != want {
		t.Errorf("shape = %s, want %s", got, want)
	}
}

func TestShapeMarksAnObjectThatCanBeNull(t *testing.T) {
	got := shapeJSON(t, `[{"plan":null},{"plan":{"id":1}}]`)
	want := `[{"plan":{"(nullable)":true,"id":"number"}}]`
	if got != want {
		t.Errorf("shape = %s, want %s", got, want)
	}
	// The null is already merged with another type when the object comes.
	got = shapeJSON(t, `[{"plan":null},{"plan":"x"},{"plan":{"id":1}}]`)
	if got != want {
		t.Errorf("shape = %s, want %s", got, want)
	}
}

func TestShapeHidesKeysThatAreData(t *testing.T) {
	got := shapeJSON(t, `{"2026-11-01":{"score":3},"2026-11-02":{"score":4,"note":"x"},
		"21718423931":{"x":1},"0123456789abcdef0123":{"y":1},"a1b2c3d4-e5f6-4789-abcd-ef0123456789":{"z":1},
		"calendarDate":"2026-11-01"}`)
	for _, leaked := range []string{"2026-11", "21718423931", "0123456789abcdef", "a1b2c3d4"} {
		if strings.Contains(got, leaked) {
			t.Errorf("shape holds %q: %s", leaked, got)
		}
	}
	var parsed map[string]any
	if err := json.NewDecoder(bytes.NewReader([]byte(got))).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 2 || parsed["calendarDate"] != "string" {
		t.Errorf("shape = %s, want the data keys merged into one", got)
	}
}

func TestSmallestIDSearchesNestedValues(t *testing.T) {
	value := decode(t, `{"plans":[{"name":"x"},{"trainingPlanId":42},{"trainingPlanId":57}]}`)
	if id, ok := smallestID(value, "trainingPlanId"); !ok || id != 42 {
		t.Errorf("smallestID = %d, %v", id, ok)
	}
	if _, ok := smallestID(value, "workoutId"); ok {
		t.Error("smallestID found a key that isn't there")
	}
}
