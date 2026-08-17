package omnisdk

// AzureScopeForTest exposes scope derivation to the package's external tests.
func AzureScopeForTest(service string) string { return azureScope(service) }
