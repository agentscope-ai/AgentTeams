package server

import (
	"reflect"
	"testing"
)

type ExactJSONEmbeddedFixture struct {
	Mode string `json:"mode,omitempty"`
}

type exactJSONSchemaFixture struct {
	ExactJSONEmbeddedFixture
	Labels map[string]string `json:"labels,omitempty"`
}

func TestValidateExactJSONFieldNamesPromotesEmbeddedFieldsAndLeavesMapKeysDynamic(t *testing.T) {
	valid := []byte(`{"mode":"sandbox","labels":{"Name":"value","Mode":"another"}}`)
	if err := validateExactJSONFieldNames(valid, reflect.TypeOf(&exactJSONSchemaFixture{})); err != nil {
		t.Fatalf("valid embedded field and dynamic map keys rejected: %v", err)
	}

	caseAlias := []byte(`{"Mode":"sandbox","labels":{"Name":"value"}}`)
	if err := validateExactJSONFieldNames(caseAlias, reflect.TypeOf(&exactJSONSchemaFixture{})); err == nil {
		t.Fatal("case alias for promoted typed field was accepted")
	}
}
