package commons

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// JSONB64Encode marshals message to JSON and returns the base64-encoded result.
func JSONB64Encode(message any) (string, error) {
	// JSON encode the message
	messageBytes, err := json.Marshal(message)
	if err != nil {
		return "", fmt.Errorf("error encoding message: %w", err)
	}

	// Base64 encode the message
	finalMessage := base64.StdEncoding.EncodeToString(messageBytes)

	return finalMessage, nil
}

// JSONB64Decode base64-decodes encodedMessage and unmarshals the JSON into target.
func JSONB64Decode(encodedMessage string, target any) error {
	// Base64 decode the message
	messageBytes, err := base64.StdEncoding.DecodeString(encodedMessage)
	if err != nil {
		return fmt.Errorf("error decoding base64: %w", err)
	}

	// JSON decode the message into the target interface
	err = json.Unmarshal(messageBytes, target)
	if err != nil {
		return fmt.Errorf("error decoding JSON: %w", err)
	}

	return nil
}
