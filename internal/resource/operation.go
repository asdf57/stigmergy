package resource

// PreserveOperationRef prevents domain status writers from dropping API coordination.
// Acquisition/completion use explicit atomic store methods instead.
func PreserveOperationRef(current, next map[string]any) map[string]any {
	result := make(map[string]any, len(next)+1)
	for key, value := range next {
		result[key] = value
	}
	delete(result, "operationRef")
	if reference := current["operationRef"]; reference != nil {
		result["operationRef"] = reference
	}
	return result
}
