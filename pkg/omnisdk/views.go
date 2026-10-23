package omnisdk

import (
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

// DocView returns the resource's declared SELECT view and dialect fallback.
// It does not execute SQL or infer request scope.
func DocView(dir, provider, service, resource string, opts ...stackqldoc.Option) (aot.View, bool, error) {
	c, err := openDocs(dir, provider, opts...)
	if err != nil {
		return nil, false, err
	}
	if views, ok := c.(interface {
		View(string, string) (aot.View, bool, error)
	}); ok {
		return views.View(service, resource)
	}
	return nil, false, nil
}
