package jitdispatcher

import "encoding/json"

var jsonMarshalIndent = func(value any) ([]byte, error) {
	return json.MarshalIndent(value, "", "  ")
}
