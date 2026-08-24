package resource

import (
	"bytes"
	"encoding/json"
)

// EqualJSON compares values by their canonical JSON representation. This
// treats numerically equivalent values such as int64(1) and float64(1) as
// equal after resources have crossed the JSON storage boundary.
func EqualJSON(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
