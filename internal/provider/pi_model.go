package provider

// IsExactPiModel reports whether model matches the exact provider/model syntax
// used by Pi native text sessions. It does not resolve or probe the model.
func IsExactPiModel(model string) bool {
	return len(model) <= 256 && piSessionModel.MatchString(model)
}
