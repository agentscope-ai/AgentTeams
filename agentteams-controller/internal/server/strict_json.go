package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
)

const workerRequestMaxBytes int64 = 1024 * 1024

// decodeStrictWorkerRequest applies one bounded JSON contract shared by
// Worker create and update. It rejects duplicate decoded object keys before
// unmarshalling so escaped-equivalent names cannot silently overwrite values.
func decodeStrictWorkerRequest(w http.ResponseWriter, r *http.Request, destination any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, workerRequestMaxBytes))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("request body must contain one JSON object")
	}
	if err := validateUniqueJSONObjectKeys(body); err != nil {
		return err
	}
	if err := validateExactJSONFieldNames(body, reflect.TypeOf(destination)); err != nil {
		return err
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain exactly one JSON object")
		}
		return err
	}
	return nil
}

var jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

func validateExactJSONFieldNames(body []byte, schema reflect.Type) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return validateExactJSONValue(value, schema, "$req")
}

func validateExactJSONValue(value any, schema reflect.Type, path string) error {
	if schema == nil || schema.Kind() == reflect.Interface || implementsJSONUnmarshaler(schema) {
		return nil
	}
	for schema.Kind() == reflect.Pointer {
		if value == nil {
			return nil
		}
		schema = schema.Elem()
		if implementsJSONUnmarshaler(schema) {
			return nil
		}
	}

	switch schema.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return nil // encoding/json reports the value-type error.
		}
		fields := exactJSONStructFields(schema)
		for name, nested := range object {
			fieldType, exists := fields[name]
			if !exists {
				return fmt.Errorf("unknown field %q at %s", name, path)
			}
			if err := validateExactJSONValue(nested, fieldType, path+"."+name); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		items, ok := value.([]any)
		if !ok {
			return nil
		}
		for index, item := range items {
			if err := validateExactJSONValue(item, schema.Elem(), fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		// Map keys are data, not typed schema fields. Env and label maps, for
		// example, intentionally accept arbitrary case-sensitive names.
		for name, nested := range object {
			if err := validateExactJSONValue(nested, schema.Elem(), path+"["+name+"]"); err != nil {
				return err
			}
		}
	}
	return nil
}

func exactJSONStructFields(schema reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	collectExactJSONStructFields(schema, fields)
	return fields
}

func collectExactJSONStructFields(schema reflect.Type, fields map[string]reflect.Type) {
	for index := 0; index < schema.NumField(); index++ {
		field := schema.Field(index)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			embedded := field.Type
			for embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct && !implementsJSONUnmarshaler(embedded) {
				collectExactJSONStructFields(embedded, fields)
				continue
			}
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
}

func implementsJSONUnmarshaler(schema reflect.Type) bool {
	if schema.Implements(jsonUnmarshalerType) {
		return true
	}
	return schema.Kind() != reflect.Pointer && reflect.PointerTo(schema).Implements(jsonUnmarshalerType)
}

func validateUniqueJSONObjectKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	opening, ok := first.(json.Delim)
	if !ok || opening != '{' {
		return errors.New("request body must contain one JSON object")
	}
	if err := walkJSONObject(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain exactly one JSON object")
		}
		return err
	}
	return nil
}

func walkJSONObject(decoder *json.Decoder) error {
	seen := map[string]struct{}{}
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return err
		}
		field, ok := fieldToken.(string)
		if !ok {
			return errors.New("JSON object field name must be a string")
		}
		if _, duplicate := seen[field]; duplicate {
			return fmt.Errorf("duplicate JSON object field %q", field)
		}
		seen[field] = struct{}{}
		value, err := decoder.Token()
		if err != nil {
			return err
		}
		if err := walkJSONValue(decoder, value); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("malformed JSON object")
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, value json.Token) error {
	delimiter, ok := value.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		return walkJSONObject(decoder)
	case '[':
		for decoder.More() {
			item, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := walkJSONValue(decoder, item); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closingDelimiter, ok := closing.(json.Delim); !ok || closingDelimiter != ']' {
			return errors.New("malformed JSON array")
		}
		return nil
	default:
		return errors.New("malformed JSON value")
	}
}
