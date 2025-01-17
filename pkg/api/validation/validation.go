/*
Copyright 2014 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package validation

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path"
	"reflect"
	"regexp"
	"strings"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/resource"
	"k8s.io/kubernetes/pkg/capabilities"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/util/intstr"
	"k8s.io/kubernetes/pkg/util/sets"
	"k8s.io/kubernetes/pkg/util/validation"
	"k8s.io/kubernetes/pkg/util/validation/field"

	"github.com/golang/glog"
)

// TODO: delete this global variable when we enable the validation of common
// fields by default.
var RepairMalformedUpdates bool = true

const isNegativeErrorMsg string = `must be greater than or equal to 0`
const fieldImmutableErrorMsg string = `field is immutable`
const cIdentifierErrorMsg string = `must be a C identifier (matching regex ` + validation.CIdentifierFmt + `): e.g. "my_name" or "MyName"`
const isNotIntegerErrorMsg string = `must be an integer`

func InclusiveRangeErrorMsg(lo, hi int) string {
	return fmt.Sprintf(`must be between %d and %d, inclusive`, lo, hi)
}

var labelValueErrorMsg string = fmt.Sprintf(`must have at most %d characters, matching regex %s: e.g. "MyValue" or ""`, validation.LabelValueMaxLength, validation.LabelValueFmt)
var qualifiedNameErrorMsg string = fmt.Sprintf(`must be a qualified name (at most %d characters, matching regex %s), with an optional DNS subdomain prefix (at most %d characters, matching regex %s) and slash (/): e.g. "MyName" or "example.com/MyName"`, validation.QualifiedNameMaxLength, validation.QualifiedNameFmt, validation.DNS1123SubdomainMaxLength, validation.DNS1123SubdomainFmt)
var DNSSubdomainErrorMsg string = fmt.Sprintf(`must be a DNS subdomain (at most %d characters, matching regex %s): e.g. "example.com"`, validation.DNS1123SubdomainMaxLength, validation.DNS1123SubdomainFmt)
var DNS1123LabelErrorMsg string = fmt.Sprintf(`must be a DNS label (at most %d characters, matching regex %s): e.g. "my-name"`, validation.DNS1123LabelMaxLength, validation.DNS1123LabelFmt)
var DNS952LabelErrorMsg string = fmt.Sprintf(`must be a DNS 952 label (at most %d characters, matching regex %s): e.g. "my-name"`, validation.DNS952LabelMaxLength, validation.DNS952LabelFmt)
var pdPartitionErrorMsg string = InclusiveRangeErrorMsg(1, 255)
var PortRangeErrorMsg string = InclusiveRangeErrorMsg(1, 65535)
var IdRangeErrorMsg string = InclusiveRangeErrorMsg(0, math.MaxInt32)
var PortNameErrorMsg string = fmt.Sprintf(`must be an IANA_SVC_NAME (at most 15 characters, matching regex %s, it must contain at least one letter [a-z], and hyphens cannot be adjacent to other hyphens): e.g. "http"`, validation.IdentifierNoHyphensBeginEndFmt)

const totalAnnotationSizeLimitB int = 256 * (1 << 10) // 256 kB

func ValidateLabelName(labelName string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if !validation.IsQualifiedName(labelName) {
		allErrs = append(allErrs, field.Invalid(fldPath, labelName, qualifiedNameErrorMsg))
	}
	return allErrs
}

// ValidateLabels validates that a set of labels are correctly defined.
func ValidateLabels(labels map[string]string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for k, v := range labels {
		allErrs = append(allErrs, ValidateLabelName(k, fldPath)...)
		if !validation.IsValidLabelValue(v) {
			allErrs = append(allErrs, field.Invalid(fldPath, v, labelValueErrorMsg))
		}
	}
	return allErrs
}

// ValidateAnnotations validates that a set of annotations are correctly defined.
func ValidateAnnotations(annotations map[string]string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	var totalSize int64
	for k, v := range annotations {
		if !validation.IsQualifiedName(strings.ToLower(k)) {
			allErrs = append(allErrs, field.Invalid(fldPath, k, qualifiedNameErrorMsg))
		}
		totalSize += (int64)(len(k)) + (int64)(len(v))
	}
	if totalSize > (int64)(totalAnnotationSizeLimitB) {
		allErrs = append(allErrs, field.TooLong(fldPath, "", totalAnnotationSizeLimitB))
	}
	return allErrs
}

// ValidateNameFunc validates that the provided name is valid for a given resource type.
// Not all resources have the same validation rules for names. Prefix is true if the
// name will have a value appended to it.
type ValidateNameFunc func(name string, prefix bool) (bool, string)

// maskTrailingDash replaces the final character of a string with a subdomain safe
// value if is a dash.
func maskTrailingDash(name string) string {
	if strings.HasSuffix(name, "-") {
		return name[:len(name)-2] + "a"
	}
	return name
}

// ValidatePodName can be used to check whether the given pod name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
func ValidatePodName(name string, prefix bool) (bool, string) {
	return NameIsDNSSubdomain(name, prefix)
}

// ValidateReplicationControllerName can be used to check whether the given replication
// controller name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
func ValidateReplicationControllerName(name string, prefix bool) (bool, string) {
	return NameIsDNSSubdomain(name, prefix)
}

// ValidateServiceName can be used to check whether the given service name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
func ValidateServiceName(name string, prefix bool) (bool, string) {
	return NameIsDNS952Label(name, prefix)
}

// ValidateNodeName can be used to check whether the given node name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
func ValidateNodeName(name string, prefix bool) (bool, string) {
	return NameIsDNSSubdomain(name, prefix)
}

// ValidateNamespaceName can be used to check whether the given namespace name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
func ValidateNamespaceName(name string, prefix bool) (bool, string) {
	return NameIsDNSLabel(name, prefix)
}

// ValidateLimitRangeName can be used to check whether the given limit range name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
func ValidateLimitRangeName(name string, prefix bool) (bool, string) {
	return NameIsDNSSubdomain(name, prefix)
}

// ValidateResourceQuotaName can be used to check whether the given
// resource quota name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
func ValidateResourceQuotaName(name string, prefix bool) (bool, string) {
	return NameIsDNSSubdomain(name, prefix)
}

// ValidateSecretName can be used to check whether the given secret name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
func ValidateSecretName(name string, prefix bool) (bool, string) {
	return NameIsDNSSubdomain(name, prefix)
}

// ValidateServiceAccountName can be used to check whether the given service account name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
func ValidateServiceAccountName(name string, prefix bool) (bool, string) {
	return NameIsDNSSubdomain(name, prefix)
}

// ValidateEndpointsName can be used to check whether the given endpoints name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
func ValidateEndpointsName(name string, prefix bool) (bool, string) {
	return NameIsDNSSubdomain(name, prefix)
}

// NameIsDNSSubdomain is a ValidateNameFunc for names that must be a DNS subdomain.
func NameIsDNSSubdomain(name string, prefix bool) (bool, string) {
	if prefix {
		name = maskTrailingDash(name)
	}
	if validation.IsDNS1123Subdomain(name) {
		return true, ""
	}
	return false, DNSSubdomainErrorMsg
}

// NameIsDNSLabel is a ValidateNameFunc for names that must be a DNS 1123 label.
func NameIsDNSLabel(name string, prefix bool) (bool, string) {
	if prefix {
		name = maskTrailingDash(name)
	}
	if validation.IsDNS1123Label(name) {
		return true, ""
	}
	return false, DNS1123LabelErrorMsg
}

// NameIsDNS952Label is a ValidateNameFunc for names that must be a DNS 952 label.
func NameIsDNS952Label(name string, prefix bool) (bool, string) {
	if prefix {
		name = maskTrailingDash(name)
	}
	if validation.IsDNS952Label(name) {
		return true, ""
	}
	return false, DNS952LabelErrorMsg
}

// Validates that given value is not negative.
func ValidateNonnegativeField(value int64, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if value < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, value, isNegativeErrorMsg))
	}
	return allErrs
}

// Validates that a Quantity is not negative
func ValidateNonnegativeQuantity(value resource.Quantity, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if value.Cmp(resource.Quantity{}) < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, value.String(), isNegativeErrorMsg))
	}
	return allErrs
}

func ValidateImmutableField(newVal, oldVal interface{}, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if !api.Semantic.DeepEqual(oldVal, newVal) {
		allErrs = append(allErrs, field.Invalid(fldPath, newVal, fieldImmutableErrorMsg))
	}
	return allErrs
}

// ValidateObjectMeta validates an object's metadata on creation. It expects that name generation has already
// been performed.
// It doesn't return an error for rootscoped resources with namespace, because namespace should already be cleared before.
// TODO: Remove calls to this method scattered in validations of specific resources, e.g., ValidatePodUpdate.
func ValidateObjectMeta(meta *api.ObjectMeta, requiresNamespace bool, nameFn ValidateNameFunc, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(meta.GenerateName) != 0 {
		if ok, qualifier := nameFn(meta.GenerateName, true); !ok {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("generateName"), meta.GenerateName, qualifier))
		}
	}
	// If the generated name validates, but the calculated value does not, it's a problem with generation, and we
	// report it here. This may confuse users, but indicates a programming bug and still must be validated.
	// If there are multiple fields out of which one is required then add a or as a separator
	if len(meta.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), "name or generateName is required"))
	} else {
		if ok, qualifier := nameFn(meta.Name, false); !ok {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), meta.Name, qualifier))
		}
	}
	if requiresNamespace {
		if len(meta.Namespace) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("namespace"), ""))
		} else if ok, _ := ValidateNamespaceName(meta.Namespace, false); !ok {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("namespace"), meta.Namespace, DNS1123LabelErrorMsg))
		}
	} else {
		if len(meta.Namespace) != 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath, "not allowed on this type"))
		}
	}
	allErrs = append(allErrs, ValidateNonnegativeField(meta.Generation, fldPath.Child("generation"))...)
	allErrs = append(allErrs, ValidateLabels(meta.Labels, fldPath.Child("labels"))...)
	allErrs = append(allErrs, ValidateAnnotations(meta.Annotations, fldPath.Child("annotations"))...)

	return allErrs
}

// ValidateObjectMetaUpdate validates an object's metadata when updated
func ValidateObjectMetaUpdate(newMeta, oldMeta *api.ObjectMeta, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if !RepairMalformedUpdates && newMeta.UID != oldMeta.UID {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("uid"), newMeta.UID, "field is immutable"))
	}
	// in the event it is left empty, set it, to allow clients more flexibility
	// TODO: remove the following code that repairs the update request when we retire the clients that modify the immutable fields.
	// Please do not copy this pattern elsewhere; validation functions should not be modifying the objects they are passed!
	if RepairMalformedUpdates {
		if len(newMeta.UID) == 0 {
			newMeta.UID = oldMeta.UID
		}
		// ignore changes to timestamp
		if oldMeta.CreationTimestamp.IsZero() {
			oldMeta.CreationTimestamp = newMeta.CreationTimestamp
		} else {
			newMeta.CreationTimestamp = oldMeta.CreationTimestamp
		}
		// an object can never remove a deletion timestamp or clear/change grace period seconds
		if !oldMeta.DeletionTimestamp.IsZero() {
			newMeta.DeletionTimestamp = oldMeta.DeletionTimestamp
		}
		if oldMeta.DeletionGracePeriodSeconds != nil && newMeta.DeletionGracePeriodSeconds == nil {
			newMeta.DeletionGracePeriodSeconds = oldMeta.DeletionGracePeriodSeconds
		}
	}

	// TODO: needs to check if newMeta==nil && oldMeta !=nil after the repair logic is removed.
	if newMeta.DeletionGracePeriodSeconds != nil && oldMeta.DeletionGracePeriodSeconds != nil && *newMeta.DeletionGracePeriodSeconds != *oldMeta.DeletionGracePeriodSeconds {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("deletionGracePeriodSeconds"), newMeta.DeletionGracePeriodSeconds, "field is immutable; may only be changed via deletion"))
	}

	// Reject updates that don't specify a resource version
	if len(newMeta.ResourceVersion) == 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("resourceVersion"), newMeta.ResourceVersion, "must be specified for an update"))
	}

	allErrs = append(allErrs, ValidateImmutableField(newMeta.Name, oldMeta.Name, fldPath.Child("name"))...)
	allErrs = append(allErrs, ValidateImmutableField(newMeta.Namespace, oldMeta.Namespace, fldPath.Child("namespace"))...)
	allErrs = append(allErrs, ValidateImmutableField(newMeta.UID, oldMeta.UID, fldPath.Child("uid"))...)
	allErrs = append(allErrs, ValidateImmutableField(newMeta.CreationTimestamp, oldMeta.CreationTimestamp, fldPath.Child("creationTimestamp"))...)

	allErrs = append(allErrs, ValidateLabels(newMeta.Labels, fldPath.Child("labels"))...)
	allErrs = append(allErrs, ValidateAnnotations(newMeta.Annotations, fldPath.Child("annotations"))...)

	return allErrs
}

func validateVolumes(volumes []api.Volume, fldPath *field.Path) (sets.String, field.ErrorList) {
	allErrs := field.ErrorList{}

	allNames := sets.String{}
	for i, vol := range volumes {
		idxPath := fldPath.Index(i)
		el := validateVolumeSource(&vol.VolumeSource, idxPath)
		if len(vol.Name) == 0 {
			el = append(el, field.Required(idxPath.Child("name"), ""))
		} else if !validation.IsDNS1123Label(vol.Name) {
			el = append(el, field.Invalid(idxPath.Child("name"), vol.Name, DNS1123LabelErrorMsg))
		} else if allNames.Has(vol.Name) {
			el = append(el, field.Duplicate(idxPath.Child("name"), vol.Name))
		}
		if len(el) == 0 {
			allNames.Insert(vol.Name)
		} else {
			allErrs = append(allErrs, el...)
		}

	}
	return allNames, allErrs
}

func validateVolumeSource(source *api.VolumeSource, fldPath *field.Path) field.ErrorList {
	numVolumes := 0
	allErrs := field.ErrorList{}
	if source.EmptyDir != nil {
		numVolumes++
		// EmptyDirs have nothing to validate
	}
	if source.HostPath != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("hostPath"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateHostPathVolumeSource(source.HostPath, fldPath.Child("hostPath"))...)
		}
	}
	if source.GitRepo != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("gitRepo"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateGitRepoVolumeSource(source.GitRepo, fldPath.Child("gitRepo"))...)
		}
	}
	if source.GCEPersistentDisk != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("gcePersistentDisk"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateGCEPersistentDiskVolumeSource(source.GCEPersistentDisk, fldPath.Child("persistentDisk"))...)
		}
	}
	if source.AWSElasticBlockStore != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("awsElasticBlockStore"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateAWSElasticBlockStoreVolumeSource(source.AWSElasticBlockStore, fldPath.Child("awsElasticBlockStore"))...)
		}
	}
	if source.Secret != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("secret"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateSecretVolumeSource(source.Secret, fldPath.Child("secret"))...)
		}
	}
	if source.NFS != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("nfs"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateNFSVolumeSource(source.NFS, fldPath.Child("nfs"))...)
		}
	}
	if source.ISCSI != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("iscsi"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateISCSIVolumeSource(source.ISCSI, fldPath.Child("iscsi"))...)
		}
	}
	if source.Glusterfs != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("glusterfs"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateGlusterfs(source.Glusterfs, fldPath.Child("glusterfs"))...)
		}
	}
	if source.Flocker != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("flocker"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateFlockerVolumeSource(source.Flocker, fldPath.Child("flocker"))...)
		}
	}
	if source.PersistentVolumeClaim != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("persistentVolumeClaim"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validatePersistentClaimVolumeSource(source.PersistentVolumeClaim, fldPath.Child("persistentVolumeClaim"))...)
		}
	}
	if source.RBD != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("rbd"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateRBDVolumeSource(source.RBD, fldPath.Child("rbd"))...)
		}
	}
	if source.Cinder != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("cinder"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateCinderVolumeSource(source.Cinder, fldPath.Child("cinder"))...)
		}
	}
	if source.CephFS != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("cephFS"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateCephFSVolumeSource(source.CephFS, fldPath.Child("cephfs"))...)
		}
	}
	if source.DownwardAPI != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("downwarAPI"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateDownwardAPIVolumeSource(source.DownwardAPI, fldPath.Child("downwardAPI"))...)
		}
	}
	if source.FC != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("fc"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateFCVolumeSource(source.FC, fldPath.Child("fc"))...)
		}
	}
	if source.FlexVolume != nil {
		numVolumes++
		allErrs = append(allErrs, validateFlexVolumeSource(source.FlexVolume, fldPath.Child("flexVolume"))...)
	}
	if numVolumes == 0 {
		allErrs = append(allErrs, field.Required(fldPath, "must specify a volume type"))
	}

	return allErrs
}

func validateHostPathVolumeSource(hostPath *api.HostPathVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(hostPath.Path) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("path"), ""))
	}
	return allErrs
}

func validateGitRepoVolumeSource(gitRepo *api.GitRepoVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(gitRepo.Repository) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("repository"), ""))
	}

	pathErrs := validateVolumeSourcePath(gitRepo.Directory, fldPath.Child("directory"))
	allErrs = append(allErrs, pathErrs...)
	return allErrs
}

func validateISCSIVolumeSource(iscsi *api.ISCSIVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(iscsi.TargetPortal) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("targetPortal"), ""))
	}
	if len(iscsi.IQN) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("iqn"), ""))
	}
	if len(iscsi.FSType) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("fsType"), ""))
	}
	if iscsi.Lun < 0 || iscsi.Lun > 255 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("lun"), iscsi.Lun, InclusiveRangeErrorMsg(0, 255)))
	}
	return allErrs
}

func validateFCVolumeSource(fc *api.FCVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(fc.TargetWWNs) < 1 {
		allErrs = append(allErrs, field.Required(fldPath.Child("targetWWNs"), ""))
	}

	if len(fc.FSType) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("fsType"), ""))
	}

	if fc.Lun == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("lun"), ""))
	} else {
		if *fc.Lun < 0 || *fc.Lun > 255 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("lun"), fc.Lun, InclusiveRangeErrorMsg(0, 255)))
		}
	}
	return allErrs
}

func validateGCEPersistentDiskVolumeSource(pd *api.GCEPersistentDiskVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(pd.PDName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("pdName"), ""))
	}
	if len(pd.FSType) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("fsType"), ""))
	}
	if pd.Partition < 0 || pd.Partition > 255 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("partition"), pd.Partition, pdPartitionErrorMsg))
	}
	return allErrs
}

func validateAWSElasticBlockStoreVolumeSource(PD *api.AWSElasticBlockStoreVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(PD.VolumeID) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumeID"), ""))
	}
	if len(PD.FSType) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("fsType"), ""))
	}
	if PD.Partition < 0 || PD.Partition > 255 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("partition"), PD.Partition, pdPartitionErrorMsg))
	}
	return allErrs
}

func validateSecretVolumeSource(secretSource *api.SecretVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(secretSource.SecretName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("secretName"), ""))
	}
	return allErrs
}

func validatePersistentClaimVolumeSource(claim *api.PersistentVolumeClaimVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(claim.ClaimName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("claimName"), ""))
	}
	return allErrs
}

func validateNFSVolumeSource(nfs *api.NFSVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(nfs.Server) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("server"), ""))
	}
	if len(nfs.Path) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("path"), ""))
	}
	if !path.IsAbs(nfs.Path) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("path"), nfs.Path, "must be an absolute path"))
	}
	return allErrs
}

func validateGlusterfs(glusterfs *api.GlusterfsVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(glusterfs.EndpointsName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("endpoints"), ""))
	}
	if len(glusterfs.Path) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("path"), ""))
	}
	return allErrs
}

func validateFlockerVolumeSource(flocker *api.FlockerVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(flocker.DatasetName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("datasetName"), ""))
	}
	if strings.Contains(flocker.DatasetName, "/") {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("datasetName"), flocker.DatasetName, "must not contain '/'"))
	}
	return allErrs
}

var validDownwardAPIFieldPathExpressions = sets.NewString("metadata.name", "metadata.namespace", "metadata.labels", "metadata.annotations")

func validateDownwardAPIVolumeSource(downwardAPIVolume *api.DownwardAPIVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, downwardAPIVolumeFile := range downwardAPIVolume.Items {
		if len(downwardAPIVolumeFile.Path) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("path"), ""))
		}
		allErrs = append(allErrs, validateVolumeSourcePath(downwardAPIVolumeFile.Path, fldPath.Child("path"))...)
		allErrs = append(allErrs, validateObjectFieldSelector(&downwardAPIVolumeFile.FieldRef, &validDownwardAPIFieldPathExpressions, fldPath.Child("fieldRef"))...)
	}
	return allErrs
}

// This validate will make sure targetPath:
// 1. is not abs path
// 2. does not contain '..'
// 3. does not start with '..'
func validateVolumeSourcePath(targetPath string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if path.IsAbs(targetPath) {
		allErrs = append(allErrs, field.Invalid(fldPath, targetPath, "must be a relative path"))
	}
	// TODO assume OS of api server & nodes are the same for now
	items := strings.Split(targetPath, string(os.PathSeparator))

	for _, item := range items {
		if item == ".." {
			allErrs = append(allErrs, field.Invalid(fldPath, targetPath, "must not contain '..'"))
		}
	}
	if strings.HasPrefix(items[0], "..") && len(items[0]) > 2 {
		allErrs = append(allErrs, field.Invalid(fldPath, targetPath, "must not start with '..'"))
	}
	return allErrs
}

func validateRBDVolumeSource(rbd *api.RBDVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(rbd.CephMonitors) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("monitors"), ""))
	}
	if len(rbd.RBDImage) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("image"), ""))
	}
	if len(rbd.FSType) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("fsType"), ""))
	}
	return allErrs
}

func validateCinderVolumeSource(cd *api.CinderVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(cd.VolumeID) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumeID"), ""))
	}
	if len(cd.FSType) == 0 || (cd.FSType != "ext3" && cd.FSType != "ext4") {
		allErrs = append(allErrs, field.Required(fldPath.Child("fsType"), ""))
	}
	return allErrs
}

func validateCephFSVolumeSource(cephfs *api.CephFSVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(cephfs.Monitors) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("monitors"), ""))
	}
	return allErrs
}

func validateFlexVolumeSource(fv *api.FlexVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(fv.Driver) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("driver"), ""))
	}
	if len(fv.FSType) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("fsType"), ""))
	}
	return allErrs
}

func ValidatePersistentVolumeName(name string, prefix bool) (bool, string) {
	return NameIsDNSSubdomain(name, prefix)
}

var supportedAccessModes = sets.NewString(string(api.ReadWriteOnce), string(api.ReadOnlyMany), string(api.ReadWriteMany))

func ValidatePersistentVolume(pv *api.PersistentVolume) field.ErrorList {
	allErrs := ValidateObjectMeta(&pv.ObjectMeta, false, ValidatePersistentVolumeName, field.NewPath("metadata"))

	specPath := field.NewPath("spec")
	if len(pv.Spec.AccessModes) == 0 {
		allErrs = append(allErrs, field.Required(specPath.Child("accessModes"), ""))
	}
	for _, mode := range pv.Spec.AccessModes {
		if !supportedAccessModes.Has(string(mode)) {
			allErrs = append(allErrs, field.NotSupported(specPath.Child("accessModes"), mode, supportedAccessModes.List()))
		}
	}

	if len(pv.Spec.Capacity) == 0 {
		allErrs = append(allErrs, field.Required(specPath.Child("capacity"), ""))
	}

	if _, ok := pv.Spec.Capacity[api.ResourceStorage]; !ok || len(pv.Spec.Capacity) > 1 {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("capacity"), pv.Spec.Capacity, []string{string(api.ResourceStorage)}))
	}
	capPath := specPath.Child("capacity")
	for r, qty := range pv.Spec.Capacity {
		allErrs = append(allErrs, validateBasicResource(qty, capPath.Key(string(r)))...)
	}

	numVolumes := 0
	if pv.Spec.HostPath != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("hostPath"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateHostPathVolumeSource(pv.Spec.HostPath, specPath.Child("hostPath"))...)
		}
	}
	if pv.Spec.GCEPersistentDisk != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("gcePersistentDisk"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateGCEPersistentDiskVolumeSource(pv.Spec.GCEPersistentDisk, specPath.Child("persistentDisk"))...)
		}
	}
	if pv.Spec.AWSElasticBlockStore != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("awsElasticBlockStore"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateAWSElasticBlockStoreVolumeSource(pv.Spec.AWSElasticBlockStore, specPath.Child("awsElasticBlockStore"))...)
		}
	}
	if pv.Spec.Glusterfs != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("glusterfs"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateGlusterfs(pv.Spec.Glusterfs, specPath.Child("glusterfs"))...)
		}
	}
	if pv.Spec.Flocker != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("flocker"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateFlockerVolumeSource(pv.Spec.Flocker, specPath.Child("flocker"))...)
		}
	}
	if pv.Spec.NFS != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("nfs"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateNFSVolumeSource(pv.Spec.NFS, specPath.Child("nfs"))...)
		}
	}
	if pv.Spec.RBD != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("rbd"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateRBDVolumeSource(pv.Spec.RBD, specPath.Child("rbd"))...)
		}
	}
	if pv.Spec.CephFS != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("cephFS"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateCephFSVolumeSource(pv.Spec.CephFS, specPath.Child("cephfs"))...)
		}
	}
	if pv.Spec.ISCSI != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("iscsi"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateISCSIVolumeSource(pv.Spec.ISCSI, specPath.Child("iscsi"))...)
		}
	}
	if pv.Spec.Cinder != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("cinder"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateCinderVolumeSource(pv.Spec.Cinder, specPath.Child("cinder"))...)
		}
	}
	if pv.Spec.FC != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("fc"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateFCVolumeSource(pv.Spec.FC, specPath.Child("fc"))...)
		}
	}
	if pv.Spec.FlexVolume != nil {
		numVolumes++
		allErrs = append(allErrs, validateFlexVolumeSource(pv.Spec.FlexVolume, specPath.Child("flexVolume"))...)
	}
	if numVolumes == 0 {
		allErrs = append(allErrs, field.Required(specPath, "must specify a volume type"))
	}
	return allErrs
}

// ValidatePersistentVolumeUpdate tests to see if the update is legal for an end user to make.
// newPv is updated with fields that cannot be changed.
func ValidatePersistentVolumeUpdate(newPv, oldPv *api.PersistentVolume) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = ValidatePersistentVolume(newPv)
	newPv.Status = oldPv.Status
	return allErrs
}

// ValidatePersistentVolumeStatusUpdate tests to see if the status update is legal for an end user to make.
// newPv is updated with fields that cannot be changed.
func ValidatePersistentVolumeStatusUpdate(newPv, oldPv *api.PersistentVolume) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newPv.ObjectMeta, &oldPv.ObjectMeta, field.NewPath("metadata"))
	if len(newPv.ResourceVersion) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("resourceVersion"), ""))
	}
	newPv.Spec = oldPv.Spec
	return allErrs
}

func ValidatePersistentVolumeClaim(pvc *api.PersistentVolumeClaim) field.ErrorList {
	allErrs := ValidateObjectMeta(&pvc.ObjectMeta, true, ValidatePersistentVolumeName, field.NewPath("metadata"))
	specPath := field.NewPath("spec")
	if len(pvc.Spec.AccessModes) == 0 {
		allErrs = append(allErrs, field.Required(specPath.Child("accessModes"), "at least 1 accessMode is required"))
	}
	for _, mode := range pvc.Spec.AccessModes {
		if mode != api.ReadWriteOnce && mode != api.ReadOnlyMany && mode != api.ReadWriteMany {
			allErrs = append(allErrs, field.NotSupported(specPath.Child("accessModes"), mode, supportedAccessModes.List()))
		}
	}
	if _, ok := pvc.Spec.Resources.Requests[api.ResourceStorage]; !ok {
		allErrs = append(allErrs, field.Required(specPath.Child("resources").Key(string(api.ResourceStorage)), ""))
	}
	return allErrs
}

func ValidatePersistentVolumeClaimUpdate(newPvc, oldPvc *api.PersistentVolumeClaim) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = ValidatePersistentVolumeClaim(newPvc)
	newPvc.Status = oldPvc.Status
	return allErrs
}

func ValidatePersistentVolumeClaimStatusUpdate(newPvc, oldPvc *api.PersistentVolumeClaim) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newPvc.ObjectMeta, &oldPvc.ObjectMeta, field.NewPath("metadata"))
	if len(newPvc.ResourceVersion) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("resourceVersion"), ""))
	}
	if len(newPvc.Spec.AccessModes) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("Spec", "accessModes"), ""))
	}
	capPath := field.NewPath("status", "capacity")
	for r, qty := range newPvc.Status.Capacity {
		allErrs = append(allErrs, validateBasicResource(qty, capPath.Key(string(r)))...)
	}
	newPvc.Spec = oldPvc.Spec
	return allErrs
}

var supportedPortProtocols = sets.NewString(string(api.ProtocolTCP), string(api.ProtocolUDP))

func validateContainerPorts(ports []api.ContainerPort, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	allNames := sets.String{}
	for i, port := range ports {
		idxPath := fldPath.Index(i)
		if len(port.Name) > 0 {
			if !validation.IsValidPortName(port.Name) {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("name"), port.Name, PortNameErrorMsg))
			} else if allNames.Has(port.Name) {
				allErrs = append(allErrs, field.Duplicate(idxPath.Child("name"), port.Name))
			} else {
				allNames.Insert(port.Name)
			}
		}
		if port.ContainerPort == 0 {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("containerPort"), port.ContainerPort, PortRangeErrorMsg))
		} else if !validation.IsValidPortNum(port.ContainerPort) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("containerPort"), port.ContainerPort, PortRangeErrorMsg))
		}
		if port.HostPort != 0 && !validation.IsValidPortNum(port.HostPort) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("hostPort"), port.HostPort, PortRangeErrorMsg))
		}
		if len(port.Protocol) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("protocol"), ""))
		} else if !supportedPortProtocols.Has(string(port.Protocol)) {
			allErrs = append(allErrs, field.NotSupported(idxPath.Child("protocol"), port.Protocol, supportedPortProtocols.List()))
		}
	}
	return allErrs
}

func validateEnv(vars []api.EnvVar, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	for i, ev := range vars {
		idxPath := fldPath.Index(i)
		if len(ev.Name) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("name"), ""))
		} else if !validation.IsCIdentifier(ev.Name) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("name"), ev.Name, cIdentifierErrorMsg))
		}
		allErrs = append(allErrs, validateEnvVarValueFrom(ev, idxPath.Child("valueFrom"))...)
	}
	return allErrs
}

var validFieldPathExpressionsEnv = sets.NewString("metadata.name", "metadata.namespace", "status.podIP")

func validateEnvVarValueFrom(ev api.EnvVar, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if ev.ValueFrom == nil {
		return allErrs
	}

	numSources := 0

	if ev.ValueFrom.FieldRef != nil {
		numSources++
		allErrs = append(allErrs, validateObjectFieldSelector(ev.ValueFrom.FieldRef, &validFieldPathExpressionsEnv, fldPath.Child("fieldRef"))...)
	}
	if ev.ValueFrom.ConfigMapKeyRef != nil {
		numSources++
		allErrs = append(allErrs, validateConfigMapKeySelector(ev.ValueFrom.ConfigMapKeyRef, fldPath.Child("configMapKeyRef"))...)
	}
	if ev.ValueFrom.SecretKeyRef != nil {
		numSources++
		allErrs = append(allErrs, validateSecretKeySelector(ev.ValueFrom.SecretKeyRef, fldPath.Child("secretKeyRef"))...)
	}

	if len(ev.Value) != 0 {
		if numSources != 0 {
			allErrs = append(allErrs, field.Invalid(fldPath, "", "may not be specified when `value` is not empty"))
		}
	} else if numSources != 1 {
		allErrs = append(allErrs, field.Invalid(fldPath, "", "may not have more than one field specified at a time"))
	}

	return allErrs
}

func validateObjectFieldSelector(fs *api.ObjectFieldSelector, expressions *sets.String, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(fs.APIVersion) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("apiVersion"), ""))
	} else if len(fs.FieldPath) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("fieldPath"), ""))
	} else {
		internalFieldPath, _, err := api.Scheme.ConvertFieldLabel(fs.APIVersion, "Pod", fs.FieldPath, "")
		if err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("fieldPath"), fs.FieldPath, fmt.Sprintf("error converting fieldPath: %v", err)))
		} else if !expressions.Has(internalFieldPath) {
			allErrs = append(allErrs, field.NotSupported(fldPath.Child("fieldPath"), internalFieldPath, expressions.List()))
		}
	}

	return allErrs
}

func validateConfigMapKeySelector(s *api.ConfigMapKeySelector, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(s.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	}
	if len(s.Key) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("key"), ""))
	} else if !IsSecretKey(s.Key) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("key"), s.Key, fmt.Sprintf("must have at most %d characters and match regex %s", validation.DNS1123SubdomainMaxLength, SecretKeyFmt)))
	}

	return allErrs
}

func validateSecretKeySelector(s *api.SecretKeySelector, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(s.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	}
	if len(s.Key) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("key"), ""))
	} else if !IsSecretKey(s.Key) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("key"), s.Key, fmt.Sprintf("must have at most %d characters and match regex %s", validation.DNS1123SubdomainMaxLength, SecretKeyFmt)))
	}

	return allErrs
}

func validateVolumeMounts(mounts []api.VolumeMount, volumes sets.String, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	for i, mnt := range mounts {
		idxPath := fldPath.Index(i)
		if len(mnt.Name) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("name"), ""))
		} else if !volumes.Has(mnt.Name) {
			allErrs = append(allErrs, field.NotFound(idxPath.Child("name"), mnt.Name))
		}
		if len(mnt.MountPath) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("mountPath"), ""))
		}
	}
	return allErrs
}

func validateProbe(probe *api.Probe, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if probe == nil {
		return allErrs
	}
	allErrs = append(allErrs, validateHandler(&probe.Handler, fldPath)...)

	allErrs = append(allErrs, ValidateNonnegativeField(int64(probe.InitialDelaySeconds), fldPath.Child("initialDelaySeconds"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(probe.TimeoutSeconds), fldPath.Child("timeoutSeconds"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(probe.PeriodSeconds), fldPath.Child("periodSeconds"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(probe.SuccessThreshold), fldPath.Child("successThreshold"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(probe.FailureThreshold), fldPath.Child("failureThreshold"))...)
	return allErrs
}

// AccumulateUniqueHostPorts extracts each HostPort of each Container,
// accumulating the results and returning an error if any ports conflict.
func AccumulateUniqueHostPorts(containers []api.Container, accumulator *sets.String, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	for ci, ctr := range containers {
		idxPath := fldPath.Index(ci)
		portsPath := idxPath.Child("ports")
		for pi := range ctr.Ports {
			idxPath := portsPath.Index(pi)
			port := ctr.Ports[pi].HostPort
			if port == 0 {
				continue
			}
			str := fmt.Sprintf("%d/%s", port, ctr.Ports[pi].Protocol)
			if accumulator.Has(str) {
				allErrs = append(allErrs, field.Duplicate(idxPath.Child("hostPort"), str))
			} else {
				accumulator.Insert(str)
			}
		}
	}
	return allErrs
}

// checkHostPortConflicts checks for colliding Port.HostPort values across
// a slice of containers.
func checkHostPortConflicts(containers []api.Container, fldPath *field.Path) field.ErrorList {
	allPorts := sets.String{}
	return AccumulateUniqueHostPorts(containers, &allPorts, fldPath)
}

func validateExecAction(exec *api.ExecAction, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	if len(exec.Command) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("command"), ""))
	}
	return allErrors
}

func validateHTTPGetAction(http *api.HTTPGetAction, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	if len(http.Path) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("path"), ""))
	}
	if http.Port.Type == intstr.Int && !validation.IsValidPortNum(http.Port.IntValue()) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("port"), http.Port, PortRangeErrorMsg))
	} else if http.Port.Type == intstr.String && !validation.IsValidPortName(http.Port.StrVal) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("port"), http.Port.StrVal, PortNameErrorMsg))
	}
	supportedSchemes := sets.NewString(string(api.URISchemeHTTP), string(api.URISchemeHTTPS))
	if !supportedSchemes.Has(string(http.Scheme)) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("scheme"), http.Scheme, fmt.Sprintf("must be one of %v", supportedSchemes.List())))
	}
	return allErrors
}

func validateTCPSocketAction(tcp *api.TCPSocketAction, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	if tcp.Port.Type == intstr.Int && !validation.IsValidPortNum(tcp.Port.IntValue()) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("port"), tcp.Port, PortRangeErrorMsg))
	} else if tcp.Port.Type == intstr.String && !validation.IsValidPortName(tcp.Port.StrVal) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("port"), tcp.Port.StrVal, PortNameErrorMsg))
	}
	return allErrors
}

func validateHandler(handler *api.Handler, fldPath *field.Path) field.ErrorList {
	numHandlers := 0
	allErrors := field.ErrorList{}
	if handler.Exec != nil {
		if numHandlers > 0 {
			allErrors = append(allErrors, field.Forbidden(fldPath.Child("exec"), "may not specify more than 1 handler type"))
		} else {
			numHandlers++
			allErrors = append(allErrors, validateExecAction(handler.Exec, fldPath.Child("exec"))...)
		}
	}
	if handler.HTTPGet != nil {
		if numHandlers > 0 {
			allErrors = append(allErrors, field.Forbidden(fldPath.Child("httpGet"), "may not specify more than 1 handler type"))
		} else {
			numHandlers++
			allErrors = append(allErrors, validateHTTPGetAction(handler.HTTPGet, fldPath.Child("httpGet"))...)
		}
	}
	if handler.TCPSocket != nil {
		if numHandlers > 0 {
			allErrors = append(allErrors, field.Forbidden(fldPath.Child("tcpSocket"), "may not specify more than 1 handler type"))
		} else {
			numHandlers++
			allErrors = append(allErrors, validateTCPSocketAction(handler.TCPSocket, fldPath.Child("tcpSocket"))...)
		}
	}
	if numHandlers == 0 {
		allErrors = append(allErrors, field.Required(fldPath, "must specify a handler type"))
	}
	return allErrors
}

func validateLifecycle(lifecycle *api.Lifecycle, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if lifecycle.PostStart != nil {
		allErrs = append(allErrs, validateHandler(lifecycle.PostStart, fldPath.Child("postStart"))...)
	}
	if lifecycle.PreStop != nil {
		allErrs = append(allErrs, validateHandler(lifecycle.PreStop, fldPath.Child("preStop"))...)
	}
	return allErrs
}

var supportedPullPolicies = sets.NewString(string(api.PullAlways), string(api.PullIfNotPresent), string(api.PullNever))

func validatePullPolicy(policy api.PullPolicy, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}

	switch policy {
	case api.PullAlways, api.PullIfNotPresent, api.PullNever:
		break
	case "":
		allErrors = append(allErrors, field.Required(fldPath, ""))
	default:
		allErrors = append(allErrors, field.NotSupported(fldPath, policy, supportedPullPolicies.List()))
	}

	return allErrors
}

func validateContainers(containers []api.Container, volumes sets.String, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(containers) == 0 {
		return append(allErrs, field.Required(fldPath, ""))
	}

	allNames := sets.String{}
	for i, ctr := range containers {
		idxPath := fldPath.Index(i)
		if len(ctr.Name) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("name"), ""))
		} else if !validation.IsDNS1123Label(ctr.Name) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("name"), ctr.Name, DNS1123LabelErrorMsg))
		} else if allNames.Has(ctr.Name) {
			allErrs = append(allErrs, field.Duplicate(idxPath.Child("name"), ctr.Name))
		} else {
			allNames.Insert(ctr.Name)
		}
		if len(ctr.Image) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("image"), ""))
		}
		if ctr.Lifecycle != nil {
			allErrs = append(allErrs, validateLifecycle(ctr.Lifecycle, idxPath.Child("lifecycle"))...)
		}
		allErrs = append(allErrs, validateProbe(ctr.LivenessProbe, idxPath.Child("livenessProbe"))...)
		// Liveness-specific validation
		if ctr.LivenessProbe != nil && ctr.LivenessProbe.SuccessThreshold != 1 {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("livenessProbe", "successThreshold"), ctr.LivenessProbe.SuccessThreshold, "must be 1"))
		}

		allErrs = append(allErrs, validateProbe(ctr.ReadinessProbe, idxPath.Child("readinessProbe"))...)
		allErrs = append(allErrs, validateContainerPorts(ctr.Ports, idxPath.Child("ports"))...)
		allErrs = append(allErrs, validateEnv(ctr.Env, idxPath.Child("env"))...)
		allErrs = append(allErrs, validateVolumeMounts(ctr.VolumeMounts, volumes, idxPath.Child("volumeMounts"))...)
		allErrs = append(allErrs, validatePullPolicy(ctr.ImagePullPolicy, idxPath.Child("imagePullPolicy"))...)
		allErrs = append(allErrs, ValidateResourceRequirements(&ctr.Resources, idxPath.Child("resources"))...)
		allErrs = append(allErrs, ValidateSecurityContext(ctr.SecurityContext, idxPath.Child("securityContext"))...)
	}
	// Check for colliding ports across all containers.
	allErrs = append(allErrs, checkHostPortConflicts(containers, fldPath)...)

	return allErrs
}

func validateRestartPolicy(restartPolicy *api.RestartPolicy, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	switch *restartPolicy {
	case api.RestartPolicyAlways, api.RestartPolicyOnFailure, api.RestartPolicyNever:
		break
	case "":
		allErrors = append(allErrors, field.Required(fldPath, ""))
	default:
		validValues := []string{string(api.RestartPolicyAlways), string(api.RestartPolicyOnFailure), string(api.RestartPolicyNever)}
		allErrors = append(allErrors, field.NotSupported(fldPath, *restartPolicy, validValues))
	}

	return allErrors
}

func validateDNSPolicy(dnsPolicy *api.DNSPolicy, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	switch *dnsPolicy {
	case api.DNSClusterFirst, api.DNSDefault:
		break
	case "":
		allErrors = append(allErrors, field.Required(fldPath, ""))
	default:
		validValues := []string{string(api.DNSClusterFirst), string(api.DNSDefault)}
		allErrors = append(allErrors, field.NotSupported(fldPath, dnsPolicy, validValues))
	}
	return allErrors
}

func validateHostNetwork(hostNetwork bool, containers []api.Container, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	if hostNetwork {
		for i, container := range containers {
			portsPath := fldPath.Index(i).Child("ports")
			for i, port := range container.Ports {
				idxPath := portsPath.Index(i)
				if port.HostPort != port.ContainerPort {
					allErrors = append(allErrors, field.Invalid(idxPath.Child("containerPort"), port.ContainerPort, "must match `hostPort` when `hostNetwork` is true"))
				}
			}
		}
	}
	return allErrors
}

// validateImagePullSecrets checks to make sure the pull secrets are well
// formed.  Right now, we only expect name to be set (it's the only field).  If
// this ever changes and someone decides to set those fields, we'd like to
// know.
func validateImagePullSecrets(imagePullSecrets []api.LocalObjectReference, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	for i, currPullSecret := range imagePullSecrets {
		idxPath := fldPath.Index(i)
		strippedRef := api.LocalObjectReference{Name: currPullSecret.Name}
		if !reflect.DeepEqual(strippedRef, currPullSecret) {
			allErrors = append(allErrors, field.Invalid(idxPath, currPullSecret, "only name may be set"))
		}
	}
	return allErrors
}

// ValidatePod tests if required fields in the pod are set.
func ValidatePod(pod *api.Pod) field.ErrorList {
	allErrs := ValidateObjectMeta(&pod.ObjectMeta, true, ValidatePodName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidatePodSpec(&pod.Spec, field.NewPath("spec"))...)
	return allErrs
}

// ValidatePodSpec tests that the specified PodSpec has valid data.
// This includes checking formatting and uniqueness.  It also canonicalizes the
// structure by setting default values and implementing any backwards-compatibility
// tricks.
func ValidatePodSpec(spec *api.PodSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	allVolumes, vErrs := validateVolumes(spec.Volumes, fldPath.Child("volumes"))
	allErrs = append(allErrs, vErrs...)
	allErrs = append(allErrs, validateContainers(spec.Containers, allVolumes, fldPath.Child("containers"))...)
	allErrs = append(allErrs, validateRestartPolicy(&spec.RestartPolicy, fldPath.Child("restartPolicy"))...)
	allErrs = append(allErrs, validateDNSPolicy(&spec.DNSPolicy, fldPath.Child("dnsPolicy"))...)
	allErrs = append(allErrs, ValidateLabels(spec.NodeSelector, fldPath.Child("nodeSelector"))...)
	allErrs = append(allErrs, ValidatePodSecurityContext(spec.SecurityContext, spec, fldPath, fldPath.Child("securityContext"))...)
	allErrs = append(allErrs, validateImagePullSecrets(spec.ImagePullSecrets, fldPath.Child("imagePullSecrets"))...)
	if len(spec.ServiceAccountName) > 0 {
		if ok, msg := ValidateServiceAccountName(spec.ServiceAccountName, false); !ok {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("serviceAccountName"), spec.ServiceAccountName, msg))
		}
	}

	if len(spec.NodeName) > 0 {
		if ok, msg := ValidateNodeName(spec.NodeName, false); !ok {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("nodeName"), spec.NodeName, msg))
		}
	}

	if spec.ActiveDeadlineSeconds != nil {
		if *spec.ActiveDeadlineSeconds <= 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("activeDeadlineSeconds"), spec.ActiveDeadlineSeconds, "must be greater than 0"))
		}
	}
	return allErrs
}

// ValidatePodSecurityContext test that the specified PodSecurityContext has valid data.
func ValidatePodSecurityContext(securityContext *api.PodSecurityContext, spec *api.PodSpec, specPath, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if securityContext != nil {
		allErrs = append(allErrs, validateHostNetwork(securityContext.HostNetwork, spec.Containers, specPath.Child("containers"))...)
		if securityContext.FSGroup != nil && !validation.IsValidGroupId(*securityContext.FSGroup) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("fsGroup"), *(securityContext.FSGroup), IdRangeErrorMsg))
		}
		if securityContext.RunAsUser != nil && !validation.IsValidUserId(*securityContext.RunAsUser) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("runAsUser"), *(securityContext.RunAsUser), IdRangeErrorMsg))
		}
		for i, gid := range securityContext.SupplementalGroups {
			if !validation.IsValidGroupId(gid) {
				supplementalGroup := fmt.Sprintf(`supplementalGroups[%d]`, i)
				allErrs = append(allErrs, field.Invalid(fldPath.Child(supplementalGroup), gid, IdRangeErrorMsg))
			}
		}
	}

	return allErrs
}

// ValidatePodUpdate tests to see if the update is legal for an end user to make. newPod is updated with fields
// that cannot be changed.
func ValidatePodUpdate(newPod, oldPod *api.Pod) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newPod.ObjectMeta, &oldPod.ObjectMeta, field.NewPath("metadata"))

	specPath := field.NewPath("spec")
	if len(newPod.Spec.Containers) != len(oldPod.Spec.Containers) {
		//TODO: Pinpoint the specific container that causes the invalid error after we have strategic merge diff
		allErrs = append(allErrs, field.Forbidden(specPath.Child("containers"), "pod updates may not add or remove containers"))
		return allErrs
	}
	pod := *newPod
	// Tricky, we need to copy the container list so that we don't overwrite the update
	var newContainers []api.Container
	for ix, container := range pod.Spec.Containers {
		container.Image = oldPod.Spec.Containers[ix].Image
		newContainers = append(newContainers, container)
	}
	pod.Spec.Containers = newContainers
	if !api.Semantic.DeepEqual(pod.Spec, oldPod.Spec) {
		//TODO: Pinpoint the specific field that causes the invalid error after we have strategic merge diff
		allErrs = append(allErrs, field.Forbidden(specPath, "pod updates may not change fields other than `containers[*].image`"))
	}

	newPod.Status = oldPod.Status
	return allErrs
}

// ValidatePodStatusUpdate tests to see if the update is legal for an end user to make. newPod is updated with fields
// that cannot be changed.
func ValidatePodStatusUpdate(newPod, oldPod *api.Pod) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newPod.ObjectMeta, &oldPod.ObjectMeta, field.NewPath("metadata"))

	// TODO: allow change when bindings are properly decoupled from pods
	if newPod.Spec.NodeName != oldPod.Spec.NodeName {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("status", "nodeName"), "may not be changed directly"))
	}

	// For status update we ignore changes to pod spec.
	newPod.Spec = oldPod.Spec

	return allErrs
}

// ValidatePodBinding tests if required fields in the pod binding are legal.
func ValidatePodBinding(binding *api.Binding) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(binding.Target.Kind) != 0 && binding.Target.Kind != "Node" {
		// TODO: When validation becomes versioned, this gets more complicated.
		allErrs = append(allErrs, field.NotSupported(field.NewPath("target", "kind"), binding.Target.Kind, []string{"Node", "<empty>"}))
	}
	if len(binding.Target.Name) == 0 {
		// TODO: When validation becomes versioned, this gets more complicated.
		allErrs = append(allErrs, field.Required(field.NewPath("target", "name"), ""))
	}

	return allErrs
}

// ValidatePodTemplate tests if required fields in the pod template are set.
func ValidatePodTemplate(pod *api.PodTemplate) field.ErrorList {
	allErrs := ValidateObjectMeta(&pod.ObjectMeta, true, ValidatePodName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidatePodTemplateSpec(&pod.Template, field.NewPath("template"))...)
	return allErrs
}

// ValidatePodTemplateUpdate tests to see if the update is legal for an end user to make. newPod is updated with fields
// that cannot be changed.
func ValidatePodTemplateUpdate(newPod, oldPod *api.PodTemplate) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&oldPod.ObjectMeta, &newPod.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidatePodTemplateSpec(&newPod.Template, field.NewPath("template"))...)
	return allErrs
}

var supportedSessionAffinityType = sets.NewString(string(api.ServiceAffinityClientIP), string(api.ServiceAffinityNone))
var supportedServiceType = sets.NewString(string(api.ServiceTypeClusterIP), string(api.ServiceTypeNodePort),
	string(api.ServiceTypeLoadBalancer))

// ValidateService tests if required fields in the service are set.
func ValidateService(service *api.Service) field.ErrorList {
	allErrs := ValidateObjectMeta(&service.ObjectMeta, true, ValidateServiceName, field.NewPath("metadata"))

	specPath := field.NewPath("spec")
	if len(service.Spec.Ports) == 0 && service.Spec.ClusterIP != api.ClusterIPNone {
		allErrs = append(allErrs, field.Required(specPath.Child("ports"), ""))
	}
	if service.Spec.Type == api.ServiceTypeLoadBalancer {
		for ix := range service.Spec.Ports {
			port := &service.Spec.Ports[ix]
			// This is a workaround for broken cloud environments that
			// over-open firewalls.  Hopefully it can go away when more clouds
			// understand containers better.
			if port.Port == 10250 {
				portPath := specPath.Child("ports").Index(ix)
				allErrs = append(allErrs, field.Invalid(portPath, port.Port, "may not expose port 10250 externally since it is used by kubelet"))
			}
		}
	}

	isHeadlessService := service.Spec.ClusterIP == api.ClusterIPNone
	allPortNames := sets.String{}
	portsPath := specPath.Child("ports")
	for i := range service.Spec.Ports {
		portPath := portsPath.Index(i)
		allErrs = append(allErrs, validateServicePort(&service.Spec.Ports[i], len(service.Spec.Ports) > 1, isHeadlessService, &allPortNames, portPath)...)
	}

	if service.Spec.Selector != nil {
		allErrs = append(allErrs, ValidateLabels(service.Spec.Selector, specPath.Child("selector"))...)
	}

	if len(service.Spec.SessionAffinity) == 0 {
		allErrs = append(allErrs, field.Required(specPath.Child("sessionAffinity"), ""))
	} else if !supportedSessionAffinityType.Has(string(service.Spec.SessionAffinity)) {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("sessionAffinity"), service.Spec.SessionAffinity, supportedSessionAffinityType.List()))
	}

	if api.IsServiceIPSet(service) {
		if ip := net.ParseIP(service.Spec.ClusterIP); ip == nil {
			allErrs = append(allErrs, field.Invalid(specPath.Child("clusterIP"), service.Spec.ClusterIP, "must be empty, 'None', or a valid IP address"))
		}
	}

	ipPath := specPath.Child("externalIPs")
	for i, ip := range service.Spec.ExternalIPs {
		idxPath := ipPath.Index(i)
		if ip == "0.0.0.0" {
			allErrs = append(allErrs, field.Invalid(idxPath, ip, "must be a valid IP address"))
		}
		allErrs = append(allErrs, validateIpIsNotLinkLocalOrLoopback(ip, idxPath)...)
	}

	if len(service.Spec.Type) == 0 {
		allErrs = append(allErrs, field.Required(specPath.Child("type"), ""))
	} else if !supportedServiceType.Has(string(service.Spec.Type)) {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("type"), service.Spec.Type, supportedServiceType.List()))
	}

	if service.Spec.Type == api.ServiceTypeLoadBalancer {
		portsPath := specPath.Child("ports")
		for i := range service.Spec.Ports {
			portPath := portsPath.Index(i)
			if !supportedPortProtocols.Has(string(service.Spec.Ports[i].Protocol)) {
				allErrs = append(allErrs, field.Invalid(portPath.Child("protocol"), service.Spec.Ports[i].Protocol, "cannot create an external load balancer with non-TCP/UDP ports"))
			}
		}
	}

	if service.Spec.Type == api.ServiceTypeClusterIP {
		portsPath := specPath.Child("ports")
		for i := range service.Spec.Ports {
			portPath := portsPath.Index(i)
			if service.Spec.Ports[i].NodePort != 0 {
				allErrs = append(allErrs, field.Invalid(portPath.Child("nodePort"), service.Spec.Ports[i].NodePort, "may not be used when `type` is 'ClusterIP'"))
			}
		}
	}

	// Check for duplicate NodePorts, considering (protocol,port) pairs
	portsPath = specPath.Child("ports")
	nodePorts := make(map[api.ServicePort]bool)
	for i := range service.Spec.Ports {
		port := &service.Spec.Ports[i]
		if port.NodePort == 0 {
			continue
		}
		portPath := portsPath.Index(i)
		var key api.ServicePort
		key.Protocol = port.Protocol
		key.NodePort = port.NodePort
		_, found := nodePorts[key]
		if found {
			allErrs = append(allErrs, field.Duplicate(portPath.Child("nodePort"), port.NodePort))
		}
		nodePorts[key] = true
	}

	return allErrs
}

func validateServicePort(sp *api.ServicePort, requireName, isHeadlessService bool, allNames *sets.String, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if requireName && len(sp.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	} else if len(sp.Name) != 0 {
		if !validation.IsDNS1123Label(sp.Name) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), sp.Name, DNS1123LabelErrorMsg))
		} else if allNames.Has(sp.Name) {
			allErrs = append(allErrs, field.Duplicate(fldPath.Child("name"), sp.Name))
		} else {
			allNames.Insert(sp.Name)
		}
	}

	if !validation.IsValidPortNum(sp.Port) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("port"), sp.Port, PortRangeErrorMsg))
	}

	if len(sp.Protocol) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("protocol"), ""))
	} else if !supportedPortProtocols.Has(string(sp.Protocol)) {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("protocol"), sp.Protocol, supportedPortProtocols.List()))
	}

	if sp.TargetPort.Type == intstr.Int && !validation.IsValidPortNum(sp.TargetPort.IntValue()) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("targetPort"), sp.TargetPort, PortRangeErrorMsg))
	}
	if sp.TargetPort.Type == intstr.String && !validation.IsValidPortName(sp.TargetPort.StrVal) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("targetPort"), sp.TargetPort, PortNameErrorMsg))
	}

	if isHeadlessService {
		if sp.TargetPort.Type == intstr.String || (sp.TargetPort.Type == intstr.Int && sp.Port != sp.TargetPort.IntValue()) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("port"), sp.Port, "must be equal to targetPort when clusterIP = None"))
		}
	}

	return allErrs
}

// ValidateServiceUpdate tests if required fields in the service are set during an update
func ValidateServiceUpdate(service, oldService *api.Service) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&service.ObjectMeta, &oldService.ObjectMeta, field.NewPath("metadata"))

	if api.IsServiceIPSet(oldService) {
		allErrs = append(allErrs, ValidateImmutableField(service.Spec.ClusterIP, oldService.Spec.ClusterIP, field.NewPath("spec", "clusterIP"))...)
	}

	allErrs = append(allErrs, ValidateService(service)...)
	return allErrs
}

// ValidateReplicationController tests if required fields in the replication controller are set.
func ValidateReplicationController(controller *api.ReplicationController) field.ErrorList {
	allErrs := ValidateObjectMeta(&controller.ObjectMeta, true, ValidateReplicationControllerName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateReplicationControllerSpec(&controller.Spec, field.NewPath("spec"))...)
	return allErrs
}

// ValidateReplicationControllerUpdate tests if required fields in the replication controller are set.
func ValidateReplicationControllerUpdate(controller, oldController *api.ReplicationController) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&controller.ObjectMeta, &oldController.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateReplicationControllerSpec(&controller.Spec, field.NewPath("spec"))...)
	return allErrs
}

// ValidateReplicationControllerStatusUpdate tests if required fields in the replication controller are set.
func ValidateReplicationControllerStatusUpdate(controller, oldController *api.ReplicationController) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&controller.ObjectMeta, &oldController.ObjectMeta, field.NewPath("metadata"))
	statusPath := field.NewPath("status")
	allErrs = append(allErrs, ValidateNonnegativeField(int64(controller.Status.Replicas), statusPath.Child("replicas"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(controller.Status.ObservedGeneration), statusPath.Child("observedGeneration"))...)
	return allErrs
}

// Validates that the given selector is non-empty.
func ValidateNonEmptySelector(selectorMap map[string]string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	selector := labels.Set(selectorMap).AsSelector()
	if selector.Empty() {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	}
	return allErrs
}

// Validates the given template and ensures that it is in accordance with the desrired selector and replicas.
func ValidatePodTemplateSpecForRC(template *api.PodTemplateSpec, selectorMap map[string]string, replicas int, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if template == nil {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else {
		selector := labels.Set(selectorMap).AsSelector()
		if !selector.Empty() {
			// Verify that the RC selector matches the labels in template.
			labels := labels.Set(template.Labels)
			if !selector.Matches(labels) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("metadata", "labels"), template.Labels, "`selector` does not match template `labels`"))
			}
		}
		allErrs = append(allErrs, ValidatePodTemplateSpec(template, fldPath)...)
		if replicas > 1 {
			allErrs = append(allErrs, ValidateReadOnlyPersistentDisks(template.Spec.Volumes, fldPath.Child("spec", "volumes"))...)
		}
		// RestartPolicy has already been first-order validated as per ValidatePodTemplateSpec().
		if template.Spec.RestartPolicy != api.RestartPolicyAlways {
			allErrs = append(allErrs, field.NotSupported(fldPath.Child("spec", "restartPolicy"), template.Spec.RestartPolicy, []string{string(api.RestartPolicyAlways)}))
		}
	}
	return allErrs
}

// ValidateReplicationControllerSpec tests if required fields in the replication controller spec are set.
func ValidateReplicationControllerSpec(spec *api.ReplicationControllerSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, ValidateNonEmptySelector(spec.Selector, fldPath.Child("selector"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(spec.Replicas), fldPath.Child("replicas"))...)
	allErrs = append(allErrs, ValidatePodTemplateSpecForRC(spec.Template, spec.Selector, spec.Replicas, fldPath.Child("template"))...)
	return allErrs
}

// ValidatePodTemplateSpec validates the spec of a pod template
func ValidatePodTemplateSpec(spec *api.PodTemplateSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, ValidateLabels(spec.Labels, fldPath.Child("labels"))...)
	allErrs = append(allErrs, ValidateAnnotations(spec.Annotations, fldPath.Child("annotations"))...)
	allErrs = append(allErrs, ValidatePodSpec(&spec.Spec, fldPath.Child("spec"))...)
	return allErrs
}

func ValidateReadOnlyPersistentDisks(volumes []api.Volume, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for i := range volumes {
		vol := &volumes[i]
		idxPath := fldPath.Index(i)
		if vol.GCEPersistentDisk != nil {
			if vol.GCEPersistentDisk.ReadOnly == false {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("gcePersistentDisk", "readOnly"), false, "must be true for replicated pods > 1; GCE PD can only be mounted on multiple machines if it is read-only"))
			}
		}
		// TODO: What to do for AWS?  It doesn't support replicas
	}
	return allErrs
}

// ValidateNode tests if required fields in the node are set.
func ValidateNode(node *api.Node) field.ErrorList {
	allErrs := ValidateObjectMeta(&node.ObjectMeta, false, ValidateNodeName, field.NewPath("metadata"))

	// Only validate spec. All status fields are optional and can be updated later.

	// external ID is required.
	if len(node.Spec.ExternalID) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "externalID"), ""))
	}

	// TODO(rjnagal): Ignore PodCIDR till its completely implemented.
	return allErrs
}

// ValidateNodeUpdate tests to make sure a node update can be applied.  Modifies oldNode.
func ValidateNodeUpdate(node, oldNode *api.Node) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&node.ObjectMeta, &oldNode.ObjectMeta, field.NewPath("metadata"))

	// TODO: Enable the code once we have better api object.status update model. Currently,
	// anyone can update node status.
	// if !api.Semantic.DeepEqual(node.Status, api.NodeStatus{}) {
	// 	allErrs = append(allErrs, field.Invalid("status", node.Status, "must be empty"))
	// }

	// Validte no duplicate addresses in node status.
	addresses := make(map[api.NodeAddress]bool)
	for i, address := range node.Status.Addresses {
		if _, ok := addresses[address]; ok {
			allErrs = append(allErrs, field.Duplicate(field.NewPath("status", "addresses").Index(i), address))
		}
		addresses[address] = true
	}

	// TODO: move reset function to its own location
	// Ignore metadata changes now that they have been tested
	oldNode.ObjectMeta = node.ObjectMeta
	// Allow users to update capacity
	oldNode.Status.Capacity = node.Status.Capacity
	// Allow the controller manager to assign a CIDR to a node.
	oldNode.Spec.PodCIDR = node.Spec.PodCIDR
	// Allow users to unschedule node
	oldNode.Spec.Unschedulable = node.Spec.Unschedulable
	// Clear status
	oldNode.Status = node.Status

	// TODO: Add a 'real' error type for this error and provide print actual diffs.
	if !api.Semantic.DeepEqual(oldNode, node) {
		glog.V(4).Infof("Update failed validation %#v vs %#v", oldNode, node)
		allErrs = append(allErrs, field.Forbidden(field.NewPath(""), "node updates may only change labels or capacity"))
	}

	return allErrs
}

// Validate compute resource typename.
// Refer to docs/design/resources.md for more details.
func validateResourceName(value string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if !validation.IsQualifiedName(value) {
		return append(allErrs, field.Invalid(fldPath, value, qualifiedNameErrorMsg))
	}

	if len(strings.Split(value, "/")) == 1 {
		if !api.IsStandardResourceName(value) {
			return append(allErrs, field.Invalid(fldPath, value, "must be a standard resource type or fully qualified"))
		}
	}

	return field.ErrorList{}
}

// ValidateLimitRange tests if required fields in the LimitRange are set.
func ValidateLimitRange(limitRange *api.LimitRange) field.ErrorList {
	allErrs := ValidateObjectMeta(&limitRange.ObjectMeta, true, ValidateLimitRangeName, field.NewPath("metadata"))

	// ensure resource names are properly qualified per docs/design/resources.md
	limitTypeSet := map[api.LimitType]bool{}
	fldPath := field.NewPath("spec", "limits")
	for i := range limitRange.Spec.Limits {
		idxPath := fldPath.Index(i)
		limit := &limitRange.Spec.Limits[i]
		_, found := limitTypeSet[limit.Type]
		if found {
			allErrs = append(allErrs, field.Duplicate(idxPath.Child("type"), limit.Type))
		}
		limitTypeSet[limit.Type] = true

		keys := sets.String{}
		min := map[string]resource.Quantity{}
		max := map[string]resource.Quantity{}
		defaults := map[string]resource.Quantity{}
		defaultRequests := map[string]resource.Quantity{}
		maxLimitRequestRatios := map[string]resource.Quantity{}

		for k, q := range limit.Max {
			allErrs = append(allErrs, validateResourceName(string(k), idxPath.Child("max").Key(string(k)))...)
			keys.Insert(string(k))
			max[string(k)] = q
		}
		for k, q := range limit.Min {
			allErrs = append(allErrs, validateResourceName(string(k), idxPath.Child("min").Key(string(k)))...)
			keys.Insert(string(k))
			min[string(k)] = q
		}

		if limit.Type == api.LimitTypePod {
			if len(limit.Default) > 0 {
				allErrs = append(allErrs, field.Forbidden(idxPath.Child("default"), "may not be specified when `type` is 'Pod'"))
			}
			if len(limit.DefaultRequest) > 0 {
				allErrs = append(allErrs, field.Forbidden(idxPath.Child("defaultRequest"), "may not be specified when `type` is 'Pod'"))
			}
		} else {
			for k, q := range limit.Default {
				allErrs = append(allErrs, validateResourceName(string(k), idxPath.Child("default").Key(string(k)))...)
				keys.Insert(string(k))
				defaults[string(k)] = q
			}
			for k, q := range limit.DefaultRequest {
				allErrs = append(allErrs, validateResourceName(string(k), idxPath.Child("defaultRequest").Key(string(k)))...)
				keys.Insert(string(k))
				defaultRequests[string(k)] = q
			}
		}

		for k, q := range limit.MaxLimitRequestRatio {
			allErrs = append(allErrs, validateResourceName(string(k), idxPath.Child("maxLimitRequestRatio").Key(string(k)))...)
			keys.Insert(string(k))
			maxLimitRequestRatios[string(k)] = q
		}

		for k := range keys {
			minQuantity, minQuantityFound := min[k]
			maxQuantity, maxQuantityFound := max[k]
			defaultQuantity, defaultQuantityFound := defaults[k]
			defaultRequestQuantity, defaultRequestQuantityFound := defaultRequests[k]
			maxRatio, maxRatioFound := maxLimitRequestRatios[k]

			if minQuantityFound && maxQuantityFound && minQuantity.Cmp(maxQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("min").Key(string(k)), minQuantity, fmt.Sprintf("min value %s is greater than max value %s", minQuantity.String(), maxQuantity.String())))
			}

			if defaultRequestQuantityFound && minQuantityFound && minQuantity.Cmp(defaultRequestQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("defaultRequest").Key(string(k)), defaultRequestQuantity, fmt.Sprintf("min value %s is greater than default request value %s", minQuantity.String(), defaultRequestQuantity.String())))
			}

			if defaultRequestQuantityFound && maxQuantityFound && defaultRequestQuantity.Cmp(maxQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("defaultRequest").Key(string(k)), defaultRequestQuantity, fmt.Sprintf("default request value %s is greater than max value %s", defaultRequestQuantity.String(), maxQuantity.String())))
			}

			if defaultRequestQuantityFound && defaultQuantityFound && defaultRequestQuantity.Cmp(defaultQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("defaultRequest").Key(string(k)), defaultRequestQuantity, fmt.Sprintf("default request value %s is greater than default limit value %s", defaultRequestQuantity.String(), defaultQuantity.String())))
			}

			if defaultQuantityFound && minQuantityFound && minQuantity.Cmp(defaultQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("default").Key(string(k)), minQuantity, fmt.Sprintf("min value %s is greater than default value %s", minQuantity.String(), defaultQuantity.String())))
			}

			if defaultQuantityFound && maxQuantityFound && defaultQuantity.Cmp(maxQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("default").Key(string(k)), maxQuantity, fmt.Sprintf("default value %s is greater than max value %s", defaultQuantity.String(), maxQuantity.String())))
			}
			if maxRatioFound && maxRatio.Cmp(*resource.NewQuantity(1, resource.DecimalSI)) < 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("maxLimitRequestRatio").Key(string(k)), maxRatio, fmt.Sprintf("ratio %s is less than 1", maxRatio.String())))
			}
			if maxRatioFound && minQuantityFound && maxQuantityFound {
				maxRatioValue := float64(maxRatio.Value())
				minQuantityValue := minQuantity.Value()
				maxQuantityValue := maxQuantity.Value()
				if maxRatio.Value() < resource.MaxMilliValue && minQuantityValue < resource.MaxMilliValue && maxQuantityValue < resource.MaxMilliValue {
					maxRatioValue = float64(maxRatio.MilliValue()) / 1000
					minQuantityValue = minQuantity.MilliValue()
					maxQuantityValue = maxQuantity.MilliValue()
				}
				maxRatioLimit := float64(maxQuantityValue) / float64(minQuantityValue)
				if maxRatioValue > maxRatioLimit {
					allErrs = append(allErrs, field.Invalid(idxPath.Child("maxLimitRequestRatio").Key(string(k)), maxRatio, fmt.Sprintf("ratio %s is greater than max/min = %f", maxRatio.String(), maxRatioLimit)))
				}
			}
		}
	}

	return allErrs
}

// ValidateServiceAccount tests if required fields in the ServiceAccount are set.
func ValidateServiceAccount(serviceAccount *api.ServiceAccount) field.ErrorList {
	allErrs := ValidateObjectMeta(&serviceAccount.ObjectMeta, true, ValidateServiceAccountName, field.NewPath("metadata"))
	return allErrs
}

// ValidateServiceAccountUpdate tests if required fields in the ServiceAccount are set.
func ValidateServiceAccountUpdate(newServiceAccount, oldServiceAccount *api.ServiceAccount) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newServiceAccount.ObjectMeta, &oldServiceAccount.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateServiceAccount(newServiceAccount)...)
	return allErrs
}

const SecretKeyFmt string = "\\.?" + validation.DNS1123LabelFmt + "(\\." + validation.DNS1123LabelFmt + ")*"

var secretKeyRegexp = regexp.MustCompile("^" + SecretKeyFmt + "$")

// IsSecretKey tests for a string that conforms to the definition of a
// subdomain in DNS (RFC 1123), except that a leading dot is allowed
func IsSecretKey(value string) bool {
	return len(value) <= validation.DNS1123SubdomainMaxLength && secretKeyRegexp.MatchString(value)
}

// ValidateSecret tests if required fields in the Secret are set.
func ValidateSecret(secret *api.Secret) field.ErrorList {
	allErrs := ValidateObjectMeta(&secret.ObjectMeta, true, ValidateSecretName, field.NewPath("metadata"))

	dataPath := field.NewPath("data")
	totalSize := 0
	for key, value := range secret.Data {
		if !IsSecretKey(key) {
			allErrs = append(allErrs, field.Invalid(dataPath.Key(key), key, fmt.Sprintf("must have at most %d characters and match regex %s", validation.DNS1123SubdomainMaxLength, SecretKeyFmt)))
		}
		totalSize += len(value)
	}
	if totalSize > api.MaxSecretSize {
		allErrs = append(allErrs, field.TooLong(dataPath, "", api.MaxSecretSize))
	}

	switch secret.Type {
	case api.SecretTypeServiceAccountToken:
		// Only require Annotations[kubernetes.io/service-account.name]
		// Additional fields (like Annotations[kubernetes.io/service-account.uid] and Data[token]) might be contributed later by a controller loop
		if value := secret.Annotations[api.ServiceAccountNameKey]; len(value) == 0 {
			allErrs = append(allErrs, field.Required(field.NewPath("metadata", "annotations").Key(api.ServiceAccountNameKey), ""))
		}
	case api.SecretTypeOpaque, "":
	// no-op
	case api.SecretTypeDockercfg:
		dockercfgBytes, exists := secret.Data[api.DockerConfigKey]
		if !exists {
			allErrs = append(allErrs, field.Required(dataPath.Key(api.DockerConfigKey), ""))
			break
		}

		// make sure that the content is well-formed json.
		if err := json.Unmarshal(dockercfgBytes, &map[string]interface{}{}); err != nil {
			allErrs = append(allErrs, field.Invalid(dataPath.Key(api.DockerConfigKey), "<secret contents redacted>", err.Error()))
		}

	default:
		// no-op
	}

	return allErrs
}

// ValidateSecretUpdate tests if required fields in the Secret are set.
func ValidateSecretUpdate(newSecret, oldSecret *api.Secret) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newSecret.ObjectMeta, &oldSecret.ObjectMeta, field.NewPath("metadata"))

	if len(newSecret.Type) == 0 {
		newSecret.Type = oldSecret.Type
	}

	allErrs = append(allErrs, ValidateImmutableField(newSecret.Type, oldSecret.Type, field.NewPath("type"))...)

	allErrs = append(allErrs, ValidateSecret(newSecret)...)
	return allErrs
}

func validateBasicResource(quantity resource.Quantity, fldPath *field.Path) field.ErrorList {
	if quantity.Value() < 0 {
		return field.ErrorList{field.Invalid(fldPath, quantity.Value(), "must be a valid resource quantity")}
	}
	return field.ErrorList{}
}

// Validates resource requirement spec.
func ValidateResourceRequirements(requirements *api.ResourceRequirements, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	limPath := fldPath.Child("limits")
	for resourceName, quantity := range requirements.Limits {
		fldPath := limPath.Key(string(resourceName))
		// Validate resource name.
		allErrs = append(allErrs, validateResourceName(string(resourceName), fldPath)...)
		if api.IsStandardResourceName(string(resourceName)) {
			allErrs = append(allErrs, validateBasicResource(quantity, fldPath.Key(string(resourceName)))...)
		}
		// Check that request <= limit.
		requestQuantity, exists := requirements.Requests[resourceName]
		if exists {
			var requestValue, limitValue int64
			requestValue = requestQuantity.Value()
			limitValue = quantity.Value()
			// Do a more precise comparison if possible (if the value won't overflow).
			if requestValue <= resource.MaxMilliValue && limitValue <= resource.MaxMilliValue {
				requestValue = requestQuantity.MilliValue()
				limitValue = quantity.MilliValue()
			}
			if limitValue < requestValue {
				allErrs = append(allErrs, field.Invalid(fldPath, quantity.String(), "must be greater than or equal to request"))
			}
		}
	}
	reqPath := fldPath.Child("requests")
	for resourceName, quantity := range requirements.Requests {
		fldPath := reqPath.Key(string(resourceName))
		// Validate resource name.
		allErrs = append(allErrs, validateResourceName(string(resourceName), fldPath)...)
		if api.IsStandardResourceName(string(resourceName)) {
			allErrs = append(allErrs, validateBasicResource(quantity, fldPath.Key(string(resourceName)))...)
		}
	}
	return allErrs
}

// ValidateResourceQuota tests if required fields in the ResourceQuota are set.
func ValidateResourceQuota(resourceQuota *api.ResourceQuota) field.ErrorList {
	allErrs := ValidateObjectMeta(&resourceQuota.ObjectMeta, true, ValidateResourceQuotaName, field.NewPath("metadata"))

	fldPath := field.NewPath("spec", "hard")
	for k, v := range resourceQuota.Spec.Hard {
		resPath := fldPath.Key(string(k))
		allErrs = append(allErrs, validateResourceName(string(k), resPath)...)
		allErrs = append(allErrs, validateResourceQuantityValue(string(k), v, resPath)...)
	}
	fldPath = field.NewPath("status", "hard")
	for k, v := range resourceQuota.Status.Hard {
		resPath := fldPath.Key(string(k))
		allErrs = append(allErrs, validateResourceName(string(k), resPath)...)
		allErrs = append(allErrs, validateResourceQuantityValue(string(k), v, resPath)...)
	}
	fldPath = field.NewPath("status", "used")
	for k, v := range resourceQuota.Status.Used {
		resPath := fldPath.Key(string(k))
		allErrs = append(allErrs, validateResourceName(string(k), resPath)...)
		allErrs = append(allErrs, validateResourceQuantityValue(string(k), v, resPath)...)
	}
	return allErrs
}

// validateResourceQuantityValue enforces that specified quantity is valid for specified resource
func validateResourceQuantityValue(resource string, value resource.Quantity, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, ValidateNonnegativeQuantity(value, fldPath)...)
	if api.IsIntegerResourceName(resource) {
		if value.MilliValue()%int64(1000) != int64(0) {
			allErrs = append(allErrs, field.Invalid(fldPath, value, isNotIntegerErrorMsg))
		}
	}
	return allErrs
}

// ValidateResourceQuotaUpdate tests to see if the update is legal for an end user to make.
// newResourceQuota is updated with fields that cannot be changed.
func ValidateResourceQuotaUpdate(newResourceQuota, oldResourceQuota *api.ResourceQuota) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newResourceQuota.ObjectMeta, &oldResourceQuota.ObjectMeta, field.NewPath("metadata"))
	fldPath := field.NewPath("spec", "hard")
	for k, v := range newResourceQuota.Spec.Hard {
		resPath := fldPath.Key(string(k))
		allErrs = append(allErrs, validateResourceName(string(k), resPath)...)
		allErrs = append(allErrs, validateResourceQuantityValue(string(k), v, resPath)...)
	}
	newResourceQuota.Status = oldResourceQuota.Status
	return allErrs
}

// ValidateResourceQuotaStatusUpdate tests to see if the status update is legal for an end user to make.
// newResourceQuota is updated with fields that cannot be changed.
func ValidateResourceQuotaStatusUpdate(newResourceQuota, oldResourceQuota *api.ResourceQuota) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newResourceQuota.ObjectMeta, &oldResourceQuota.ObjectMeta, field.NewPath("metadata"))
	if len(newResourceQuota.ResourceVersion) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("resourceVersion"), ""))
	}
	fldPath := field.NewPath("status", "hard")
	for k, v := range newResourceQuota.Status.Hard {
		resPath := fldPath.Key(string(k))
		allErrs = append(allErrs, validateResourceName(string(k), resPath)...)
		allErrs = append(allErrs, validateResourceQuantityValue(string(k), v, resPath)...)
	}
	fldPath = field.NewPath("status", "used")
	for k, v := range newResourceQuota.Status.Used {
		resPath := fldPath.Key(string(k))
		allErrs = append(allErrs, validateResourceName(string(k), resPath)...)
		allErrs = append(allErrs, validateResourceQuantityValue(string(k), v, resPath)...)
	}
	newResourceQuota.Spec = oldResourceQuota.Spec
	return allErrs
}

// ValidateNamespace tests if required fields are set.
func ValidateNamespace(namespace *api.Namespace) field.ErrorList {
	allErrs := ValidateObjectMeta(&namespace.ObjectMeta, false, ValidateNamespaceName, field.NewPath("metadata"))
	for i := range namespace.Spec.Finalizers {
		allErrs = append(allErrs, validateFinalizerName(string(namespace.Spec.Finalizers[i]), field.NewPath("spec", "finalizers"))...)
	}
	return allErrs
}

// Validate finalizer names
func validateFinalizerName(stringValue string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if !validation.IsQualifiedName(stringValue) {
		return append(allErrs, field.Invalid(fldPath, stringValue, qualifiedNameErrorMsg))
	}

	if len(strings.Split(stringValue, "/")) == 1 {
		if !api.IsStandardFinalizerName(stringValue) {
			return append(allErrs, field.Invalid(fldPath, stringValue, fmt.Sprintf("name is neither a standard finalizer name nor is it fully qualified")))
		}
	}

	return field.ErrorList{}
}

// ValidateNamespaceUpdate tests to make sure a namespace update can be applied.
// newNamespace is updated with fields that cannot be changed
func ValidateNamespaceUpdate(newNamespace *api.Namespace, oldNamespace *api.Namespace) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newNamespace.ObjectMeta, &oldNamespace.ObjectMeta, field.NewPath("metadata"))
	newNamespace.Spec.Finalizers = oldNamespace.Spec.Finalizers
	newNamespace.Status = oldNamespace.Status
	return allErrs
}

// ValidateNamespaceStatusUpdate tests to see if the update is legal for an end user to make. newNamespace is updated with fields
// that cannot be changed.
func ValidateNamespaceStatusUpdate(newNamespace, oldNamespace *api.Namespace) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newNamespace.ObjectMeta, &oldNamespace.ObjectMeta, field.NewPath("metadata"))
	newNamespace.Spec = oldNamespace.Spec
	if newNamespace.DeletionTimestamp.IsZero() {
		if newNamespace.Status.Phase != api.NamespaceActive {
			allErrs = append(allErrs, field.Invalid(field.NewPath("status", "Phase"), newNamespace.Status.Phase, "may only be 'Active' if `deletionTimestamp` is empty"))
		}
	} else {
		if newNamespace.Status.Phase != api.NamespaceTerminating {
			allErrs = append(allErrs, field.Invalid(field.NewPath("status", "Phase"), newNamespace.Status.Phase, "may only be 'Terminating' if `deletionTimestamp` is not empty"))
		}
	}
	return allErrs
}

// ValidateNamespaceFinalizeUpdate tests to see if the update is legal for an end user to make.
// newNamespace is updated with fields that cannot be changed.
func ValidateNamespaceFinalizeUpdate(newNamespace, oldNamespace *api.Namespace) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newNamespace.ObjectMeta, &oldNamespace.ObjectMeta, field.NewPath("metadata"))

	fldPath := field.NewPath("spec", "finalizers")
	for i := range newNamespace.Spec.Finalizers {
		idxPath := fldPath.Index(i)
		allErrs = append(allErrs, validateFinalizerName(string(newNamespace.Spec.Finalizers[i]), idxPath)...)
	}
	newNamespace.Status = oldNamespace.Status
	return allErrs
}

// ValidateEndpoints tests if required fields are set.
func ValidateEndpoints(endpoints *api.Endpoints) field.ErrorList {
	allErrs := ValidateObjectMeta(&endpoints.ObjectMeta, true, ValidateEndpointsName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateEndpointSubsets(endpoints.Subsets, field.NewPath("subsets"))...)
	return allErrs
}

func validateEndpointSubsets(subsets []api.EndpointSubset, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	for i := range subsets {
		ss := &subsets[i]
		idxPath := fldPath.Index(i)

		if len(ss.Addresses) == 0 && len(ss.NotReadyAddresses) == 0 {
			//TODO: consider adding a RequiredOneOf() error for this and similar cases
			allErrs = append(allErrs, field.Required(idxPath, "must specify `addresses` or `notReadyAddresses`"))
		}
		if len(ss.Ports) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("ports"), ""))
		}
		for addr := range ss.Addresses {
			allErrs = append(allErrs, validateEndpointAddress(&ss.Addresses[addr], idxPath.Child("addresses").Index(addr))...)
		}
		for port := range ss.Ports {
			allErrs = append(allErrs, validateEndpointPort(&ss.Ports[port], len(ss.Ports) > 1, idxPath.Child("ports").Index(port))...)
		}
	}

	return allErrs
}

func validateEndpointAddress(address *api.EndpointAddress, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if !validation.IsValidIPv4(address.IP) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("ip"), address.IP, "must be a valid IPv4 address"))
		return allErrs
	}
	return validateIpIsNotLinkLocalOrLoopback(address.IP, fldPath.Child("ip"))
}

func validateIpIsNotLinkLocalOrLoopback(ipAddress string, fldPath *field.Path) field.ErrorList {
	// We disallow some IPs as endpoints or external-ips.  Specifically, loopback addresses are
	// nonsensical and link-local addresses tend to be used for node-centric purposes (e.g. metadata service).
	allErrs := field.ErrorList{}
	ip := net.ParseIP(ipAddress)
	if ip == nil {
		allErrs = append(allErrs, field.Invalid(fldPath, ipAddress, "must be a valid IP address"))
		return allErrs
	}
	if ip.IsLoopback() {
		allErrs = append(allErrs, field.Invalid(fldPath, ipAddress, "may not be in the loopback range (127.0.0.0/8)"))
	}
	if ip.IsLinkLocalUnicast() {
		allErrs = append(allErrs, field.Invalid(fldPath, ipAddress, "may not be in the link-local range (169.254.0.0/16)"))
	}
	if ip.IsLinkLocalMulticast() {
		allErrs = append(allErrs, field.Invalid(fldPath, ipAddress, "may not be in the link-local multicast range (224.0.0.0/24)"))
	}
	return allErrs
}

func validateEndpointPort(port *api.EndpointPort, requireName bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if requireName && len(port.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	} else if len(port.Name) != 0 {
		if !validation.IsDNS1123Label(port.Name) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), port.Name, DNS1123LabelErrorMsg))
		}
	}
	if !validation.IsValidPortNum(port.Port) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("port"), port.Port, PortRangeErrorMsg))
	}
	if len(port.Protocol) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("protocol"), ""))
	} else if !supportedPortProtocols.Has(string(port.Protocol)) {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("protocol"), port.Protocol, supportedPortProtocols.List()))
	}
	return allErrs
}

// ValidateEndpointsUpdate tests to make sure an endpoints update can be applied.
func ValidateEndpointsUpdate(newEndpoints, oldEndpoints *api.Endpoints) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newEndpoints.ObjectMeta, &oldEndpoints.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, validateEndpointSubsets(newEndpoints.Subsets, field.NewPath("subsets"))...)
	return allErrs
}

// ValidateSecurityContext ensure the security context contains valid settings
func ValidateSecurityContext(sc *api.SecurityContext, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	//this should only be true for testing since SecurityContext is defaulted by the api
	if sc == nil {
		return allErrs
	}

	if sc.Privileged != nil {
		if *sc.Privileged && !capabilities.Get().AllowPrivileged {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("privileged"), "disallowed by policy"))
		}
	}

	if sc.RunAsUser != nil {
		if *sc.RunAsUser < 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("runAsUser"), *sc.RunAsUser, isNegativeErrorMsg))
		}
	}
	return allErrs
}

func ValidatePodLogOptions(opts *api.PodLogOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if opts.TailLines != nil && *opts.TailLines < 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("tailLines"), *opts.TailLines, isNegativeErrorMsg))
	}
	if opts.LimitBytes != nil && *opts.LimitBytes < 1 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("limitBytes"), *opts.LimitBytes, "must be greater than 0"))
	}
	switch {
	case opts.SinceSeconds != nil && opts.SinceTime != nil:
		allErrs = append(allErrs, field.Forbidden(field.NewPath(""), "at most one of `sinceTime` or `sinceSeconds` may be specified"))
	case opts.SinceSeconds != nil:
		if *opts.SinceSeconds < 1 {
			allErrs = append(allErrs, field.Invalid(field.NewPath("sinceSeconds"), *opts.SinceSeconds, "must be greater than 0"))
		}
	}
	return allErrs
}

// ValidateLoadBalancerStatus validates required fields on a LoadBalancerStatus
func ValidateLoadBalancerStatus(status *api.LoadBalancerStatus, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for i, ingress := range status.Ingress {
		idxPath := fldPath.Child("ingress").Index(i)
		if len(ingress.IP) > 0 {
			if isIP := (net.ParseIP(ingress.IP) != nil); !isIP {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("ip"), ingress.IP, "must be a valid IP address"))
			}
		}
		if len(ingress.Hostname) > 0 {
			if valid, errMsg := NameIsDNSSubdomain(ingress.Hostname, false); !valid {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("hostname"), ingress.Hostname, errMsg))
			}
			if isIP := (net.ParseIP(ingress.Hostname) != nil); isIP {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("hostname"), ingress.Hostname, "must be a DNS name, not an IP address"))
			}
		}
	}
	return allErrs
}
