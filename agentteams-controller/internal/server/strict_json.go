package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
