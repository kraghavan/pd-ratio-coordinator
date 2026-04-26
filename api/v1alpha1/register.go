package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "llmd.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add functions to this group's scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&PDRatioPolicy{}, &PDRatioPolicyList{})
}

// DeepCopyObject implements runtime.Object.
func (in *PDRatioPolicy) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(PDRatioPolicy)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies all properties into another PDRatioPolicy.
func (in *PDRatioPolicy) DeepCopyInto(out *PDRatioPolicy) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopyObject implements runtime.Object for PDRatioPolicyList.
func (in *PDRatioPolicyList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(PDRatioPolicyList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies all properties into another PDRatioPolicyList.
func (in *PDRatioPolicyList) DeepCopyInto(out *PDRatioPolicyList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		items := make([]PDRatioPolicy, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&items[i])
		}
		out.Items = items
	}
}

// DeepCopyInto for Status
func (in *PDRatioPolicyStatus) DeepCopyInto(out *PDRatioPolicyStatus) {
	*out = *in
	if in.LastScaleTime != nil {
		t := *in.LastScaleTime
		out.LastScaleTime = &t
	}
	if in.Conditions != nil {
		conds := make([]interface{}, len(in.Conditions))
		_ = conds
		out.Conditions = append([]interface{}{}, in.Conditions...)
	}
}
