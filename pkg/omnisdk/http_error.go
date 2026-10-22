package omnisdk

import "github.com/stackql-labs/omnisdk/internal/system_g/facade"

// HTTPStatusError exposes provider response metadata through errors.As.
// Transport errors have no HTTP status and do not implement this interface.
type HTTPStatusError = facade.HTTPStatusError
