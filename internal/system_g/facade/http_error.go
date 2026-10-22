package facade

// HTTPStatusError is a provider response with a non-success status, distinct
// from a transport failure that produced no response.
type HTTPStatusError interface {
	error
	StatusCode() int
	ResponseBody() string
	RequestURL() string
}
