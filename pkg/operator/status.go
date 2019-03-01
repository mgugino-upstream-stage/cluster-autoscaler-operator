package operator

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	configv1 "github.com/openshift/api/config/v1"
	osconfigv1 "github.com/openshift/api/config/v1"
	osconfig "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/openshift/cluster-autoscaler-operator/pkg/version"
	cvorm "github.com/openshift/cluster-version-operator/lib/resourcemerge"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"reflect"
	"strings"
)

// Reason messages used in status conditions.
const (
	ReasonEmpty             = ""
	ReasonMissingDependency = "MissingDependency"
	ReasonSyncing           = "SyncingResources"
)

// StatusReporter reports the status of the operator to the OpenShift
// cluster-version-operator via ClusterOperator resource status.
type StatusReporter struct {
	client         osconfig.Interface
	relatedObjects []configv1.ObjectReference
}

// NewStatusReporter returns a new StatusReporter instance.
func NewStatusReporter(cfg *rest.Config, relatedObjects []configv1.ObjectReference) (*StatusReporter, error) {
	var err error
	reporter := &StatusReporter{
		relatedObjects: relatedObjects,
	}

	// Create a client for OpenShift config objects.
	reporter.client, err = osconfig.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	return reporter, nil
}

// GetOrCreateClusterOperator gets, or if necessary, creates the
// operator's ClusterOperator object and returns it.
func (r *StatusReporter) GetOrCreateClusterOperator() (*configv1.ClusterOperator, error) {
	clusterOperator := &configv1.ClusterOperator{
		ObjectMeta: metav1.ObjectMeta{
			Name: OperatorName,
		},
	}

	existing, err := r.client.ConfigV1().ClusterOperators().
		Get(OperatorName, metav1.GetOptions{})

	if errors.IsNotFound(err) {
		return r.client.ConfigV1().ClusterOperators().Create(clusterOperator)
	}

	return existing, err
}

func (r *StatusReporter) IsDifferentVersions(desiredVersions []osconfigv1.OperandVersion) (bool, error) {
	co, err := r.GetOrCreateClusterOperator()
	if err != nil {
		return false, err
	}
	currentVersions := co.Status.Versions
	return !reflect.DeepEqual(desiredVersions, currentVersions), nil
}

// ApplyConditions applies the given conditions to the ClusterOperator
// resource's status.
func (r *StatusReporter) ApplyConditions(conds []configv1.ClusterOperatorStatusCondition) error {
	status := configv1.ClusterOperatorStatus{
		Versions: []configv1.OperandVersion{
			{
				Name:    "cluster-autoscaler",
				Version: version.Raw,
			},
		},
		RelatedObjects: r.relatedObjects,
	}

	for _, c := range conds {
		cvorm.SetOperatorStatusCondition(&status.Conditions, c)
	}

	co, err := r.GetOrCreateClusterOperator()
	if err != nil {
		return err
	}

	if !equality.Semantic.DeepEqual(co.Status, status) {
		co.Status = status
		_, err = r.client.ConfigV1().ClusterOperators().UpdateStatus(co)
	}

	return err
}

// Available reports the operator as available, not progressing, and
// not failing -- optionally setting a reason and message.
func (r *StatusReporter) Available(reason, message string) error {
	conditions := []configv1.ClusterOperatorStatusCondition{
		{
			Type:    configv1.OperatorAvailable,
			Status:  configv1.ConditionTrue,
			Reason:  reason,
			Message: message,
		},
		{
			Type:   configv1.OperatorProgressing,
			Status: configv1.ConditionFalse,
		},
		{
			Type:   configv1.OperatorFailing,
			Status: configv1.ConditionFalse,
		},
	}

	return r.ApplyConditions(conditions)
}

// Fail reports the operator as failing but available, and not
// progressing -- optionally setting a reason and message.
func (r *StatusReporter) Fail(reason, message string) error {
	conditions := []configv1.ClusterOperatorStatusCondition{
		{
			Type:   configv1.OperatorAvailable,
			Status: configv1.ConditionTrue,
		},
		{
			Type:   configv1.OperatorProgressing,
			Status: configv1.ConditionFalse,
		},
		{
			Type:    configv1.OperatorFailing,
			Status:  configv1.ConditionTrue,
			Reason:  reason,
			Message: message,
		},
	}

	return r.ApplyConditions(conditions)
}

// Progressing reports the operator as progressing but available, and not
// failing -- optionally setting a reason and message.
func (r *StatusReporter) Progressing(reason, message string) error {
	conditions := []configv1.ClusterOperatorStatusCondition{
		{
			Type:   configv1.OperatorAvailable,
			Status: configv1.ConditionTrue,
		},
		{
			Type:    configv1.OperatorProgressing,
			Status:  configv1.ConditionTrue,
			Reason:  reason,
			Message: message,
		},
		{
			Type:   configv1.OperatorFailing,
			Status: configv1.ConditionFalse,
		},
	}

	return r.ApplyConditions(conditions)
}

// Report checks the status of dependencies and reports the operator's
// status.  It will poll until stopCh is closed or prerequisites are
// satisfied, in which case it will report the operator as available
// and return.
func (r *StatusReporter) Report(stopCh <-chan struct{}) error {
	interval := 15 * time.Second

	// Poll the status of our prerequisites and set our status
	// accordingly.  Rather than return errors and stop polling, most
	// errors here should just be reported in the status message.
	pollFunc := func() (bool, error) {
		ok, err := r.CheckMachineAPI()
		if err != nil {
			r.Fail(ReasonMissingDependency, fmt.Sprintf("error checking machine-api operator status %v", err))
			return false, nil
		}

		if ok {
			// desiredVersions will be replaced by value from CVO
			desiredVersions := []osconfigv1.OperandVersion{
				{
					Name:    "operator",
					Version: version.Raw,
				},
			}
			isProgress, err2 := r.IsDifferentVersions(desiredVersions)
			if err2 != nil {
				r.Fail(ReasonEmpty, fmt.Sprintf("error checking cluster-autoscaler-operator version %v", err2))
				return false, nil
			}
			if isProgress {
				r.Progressing(ReasonSyncing, fmt.Sprintf("Syncing to version %v", printOperandVersions(desiredVersions)))
				return false, nil
			}
			r.Available(ReasonEmpty, "")
			return true, nil
		}

		r.Fail(ReasonMissingDependency, "machine-api operator not ready")
		return false, nil
	}

	return wait.PollImmediateUntil(interval, pollFunc, stopCh)
}

// CheckMachineAPI checks the status of the machine-api-operator as
// reported to the CVO.  It returns true if the operator is available
// and not failing.
func (r *StatusReporter) CheckMachineAPI() (bool, error) {
	mao, err := r.client.ConfigV1().ClusterOperators().
		Get("machine-api", metav1.GetOptions{})

	if err != nil {
		glog.Errorf("failed to get dependency machine-api status: %v", err)
		return false, err
	}

	conds := mao.Status.Conditions

	if cvorm.IsOperatorStatusConditionTrue(conds, configv1.OperatorAvailable) &&
		cvorm.IsOperatorStatusConditionFalse(conds, configv1.OperatorFailing) {
		return true, nil
	}

	glog.Infof("machine-api-operator not ready yet")
	return false, nil
}

func printOperandVersions(desiredVersions []osconfigv1.OperandVersion) string {
	versionsOutput := []string{}
	for _, operand := range desiredVersions {
		versionsOutput = append(versionsOutput, fmt.Sprintf("%s: %s", operand.Name, operand.Version))
	}
	return strings.Join(versionsOutput, ", ")
}
