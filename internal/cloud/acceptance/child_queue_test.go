package acceptance

import "github.com/goobers/goobers/internal/childworkflow"

// Cloud custody is the child request path's second backend (#8085).
var _ childworkflow.ChildQueue = (*Store)(nil)
