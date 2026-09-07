package wasitter_test

// mustQueryResult keeps result assertions concise while making execution errors
// fail the test, instead of treating errors as empty match collections.
func mustQueryResult[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}
