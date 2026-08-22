package memory

// TrainingPair is a learned mapping from a raw SMS topic (merchant/counterparty name) to the
// enriched expense fields the agent should reuse for future transactions with a similar topic.
type TrainingPair struct {
	RawTopic      string
	Name          string
	CategoryFK    string
	SubCategoryFK string
	WalletFK      string
	Income        bool
}

// Candidate is a training pair returned by a similarity search, together with the trigram
// similarity score (0..1) against the queried topic.
type Candidate struct {
	TrainingPair
	Score float64
}
