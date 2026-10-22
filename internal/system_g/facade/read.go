package facade

// ReadEffects reports row controls already consumed by an upstream API. A
// downstream relational stage must not apply the same offset a second time.
type ReadEffects interface{ AppliedOffset() int }
