package secretstore

import (
	"errors"
)

var ErrNotFound = errors.New("secret not found")
var ErrUnavailable = errors.New("OS secret store unavailable")
