package operator

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/goobers/goobers/api/v1alpha1"
)

// NewScheme builds a runtime scheme with the core/apps types plus the Goobers
// CRD types the operator reconciles. The v1alpha1 package intentionally ships
// types only (no SchemeBuilder), so registration is done here.
func NewScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	s.AddKnownTypes(v1alpha1.GroupVersion,
		&v1alpha1.Gaggle{}, &v1alpha1.GaggleList{},
		&v1alpha1.Goober{}, &v1alpha1.GooberList{},
	)
	metav1.AddToGroupVersion(s, v1alpha1.GroupVersion)
	return s, nil
}
