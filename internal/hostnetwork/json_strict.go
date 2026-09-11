package hostnetwork

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := inspectJSONValue(decoder); err != nil {
		return fmt.Errorf("network evidence JSON identity is invalid: %w", err)
	}
	return nil
}

func inspectJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	if delimiter == '[' {
		for decoder.More() {
			if err := inspectJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	if delimiter != '{' {
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return inspectJSONObject(decoder)
}

func inspectJSONObject(decoder *json.Decoder) error {
	keys := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, valid := token.(string)
		if !valid {
			return fmt.Errorf("JSON object key is not a string")
		}
		if _, duplicate := keys[key]; duplicate {
			return fmt.Errorf("duplicate JSON field %q", key)
		}
		keys[key] = struct{}{}
		if err := inspectJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}

func requireJSONEnd(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("network evidence has trailing JSON value")
		}
		return fmt.Errorf("decode network evidence trailing content: %w", err)
	}
	return nil
}
