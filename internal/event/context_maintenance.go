package event

// ContextMaintenance is the typed, content-free maintenance receipt.
type ContextMaintenance struct {
	Status               string `json:"status,omitempty"`
	Action               string `json:"action,omitempty"`
	Trigger              string `json:"trigger,omitempty"`
	OperationID          string `json:"operationId,omitempty"`
	InputTokens          int    `json:"inputTokens,omitempty"`
	ResultTokens         int    `json:"resultTokens,omitempty"`
	SavedTokens          int    `json:"savedTokens,omitempty"`
	AffectedToolResults  int    `json:"affectedToolResults,omitempty"`
	ProjectionVersion    uint64 `json:"projectionVersion,omitempty"`
	ProjectionGeneration uint64 `json:"projectionGeneration,omitempty"`
	CacheGeneration      uint64 `json:"cacheGeneration,omitempty"`
	CacheBreak           bool   `json:"cacheBreak,omitempty"`
	Mode                 string `json:"mode,omitempty"`
	CoveredCanonicalFrom int    `json:"coveredCanonicalFrom,omitempty"`
	CoveredCanonicalTo   int    `json:"coveredCanonicalTo,omitempty"`
	FoldUnits            int    `json:"foldUnits,omitempty"`
	SummaryPromptTokens  int    `json:"summaryPromptTokens,omitempty"`
	SummaryOutputTokens  int    `json:"summaryOutputTokens,omitempty"`
	SummaryLatencyMS     int64  `json:"summaryLatencyMs,omitempty"`
	ArchiveBytes         int    `json:"archiveBytes,omitempty"`
	ArchiveRefsCount     int    `json:"archiveRefsCount,omitempty"`
	KeptRecentToolGroups int    `json:"keptRecentToolGroups,omitempty"`
	ProviderWindowSource string `json:"providerWindowSource,omitempty"`
	IrreducibleReason    string `json:"irreducibleReason,omitempty"`
	Reason               string `json:"reason,omitempty"`
}
