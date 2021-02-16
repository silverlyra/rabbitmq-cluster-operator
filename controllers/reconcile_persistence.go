package controllers

import (
	"errors"
	"context"
	"fmt"
	"github.com/go-logr/logr"
	rabbitmqv1beta1 "github.com/rabbitmq/cluster-operator/api/v1beta1"
	"github.com/rabbitmq/cluster-operator/internal/resource"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	k8sresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)


func (r *RabbitmqClusterReconciler) reconcilePVC(ctx context.Context, builder resource.ResourceBuilder, cluster *rabbitmqv1beta1.RabbitmqCluster, resource client.Object) error {
	logger := ctrl.LoggerFrom(ctx)

	switch sts := resource.(type) {
	case *appsv1.StatefulSet:
		current, err := r.statefulSet(ctx, cluster)
		if client.IgnoreNotFound(err) != nil {
			return err
		} else if k8serrors.IsNotFound(err) {
			logger.Info("statefulSet not created yet, skipping checks to expand PersistentVolumeClaims")
			return nil
		}

		if err := builder.Update(sts); err != nil {
			return err
		}

		resize, err := r.needsPVCResize(current, sts)

		if err != nil {
			return err
		}

		if resize {
			if err := r.expandPVC(ctx, cluster, current, sts); err != nil {
				logger.Error(err, "Failed to expand PersistentVolumeClaims", "statefulSet", cluster.ChildResourceName("server"))
				return err
			}
		}
	}
	return nil
}

func (r *RabbitmqClusterReconciler) expandPVC(ctx context.Context, rmq *rabbitmqv1beta1.RabbitmqCluster, current, desired *appsv1.StatefulSet) error {
	logger := ctrl.LoggerFrom(ctx)

	currentCapacity, err := persistenceStorageCapacity(current.Spec.VolumeClaimTemplates)
	if err != nil {
		return err
	}

	desiredCapacity, err := persistenceStorageCapacity(desired.Spec.VolumeClaimTemplates)
	if err != nil {
		return err
	}

	logger.Info(fmt.Sprintf("updating storage capacity from %v to %v", currentCapacity, desiredCapacity))

	if err := r.deleteSts(ctx, rmq); err != nil {
		return err
	}

	if err := r.updatePVC(ctx, rmq,  *current.Spec.Replicas, desiredCapacity); err != nil {
		return err
	}

	return nil
}

func (r *RabbitmqClusterReconciler) updatePVC(ctx context.Context, rmq *rabbitmqv1beta1.RabbitmqCluster, replicas int32, desiredCapacity k8sresource.Quantity) error {
	logger := ctrl.LoggerFrom(ctx)
	logger.Info("expanding PersistentVolumeClaims")

	for i := 0; i < int(replicas); i++ {
		PVCName := rmq.PVCName(i)
		PVC := corev1.PersistentVolumeClaim{}

		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: rmq.Namespace, Name: PVCName}, &PVC); err != nil {
			logger.Error(err, "failed to get PersistentVolumeClaim")
			return err
		}
		PVC.Spec.Resources.Requests[corev1.ResourceStorage] = desiredCapacity
		if err := r.Client.Update(ctx, &PVC, &client.UpdateOptions{}); err != nil {
			logger.Error(err, "failed to update PersistentVolumeClaim")
			return err
		}
		logger.Info("successfully expanded", "PVC", PVCName)
	}
	return nil
}

func (r *RabbitmqClusterReconciler) needsPVCResize(current, desired *appsv1.StatefulSet) (bool, error) {
	currentCapacity, err := persistenceStorageCapacity(current.Spec.VolumeClaimTemplates)
	if err != nil {
		return false, err
	}

	desiredCapacity, err := persistenceStorageCapacity(desired.Spec.VolumeClaimTemplates)
	if err != nil {
		return false, err
	}

	if currentCapacity.Cmp(desiredCapacity) != 0 {
		return true, nil
	}

	return false, nil
}

func persistenceStorageCapacity(templates []corev1.PersistentVolumeClaim)  (k8sresource.Quantity, error) {
	for _, t := range templates {
		if t.Name == "persistence" {
			return t.Spec.Resources.Requests[corev1.ResourceStorage], nil
		}
	}
	return k8sresource.Quantity{}, errors.New("cannot find PersistentVolumeClaim 'persistence'")
}


// deleteSts deletes a sts without deleting pods and PVCs
// using DeletePropagationPolicy set to 'Orphan'
func (r *RabbitmqClusterReconciler) deleteSts(ctx context.Context, rmq *rabbitmqv1beta1.RabbitmqCluster) error {
	logger := ctrl.LoggerFrom(ctx)
	logger.Info("deleting statefulSet (pods won't be deleted)", "statefulSet", rmq.ChildResourceName("server"))
	deletePropagationPolicy := metav1.DeletePropagationOrphan
	deleteOptions := &client.DeleteOptions{PropagationPolicy: &deletePropagationPolicy}
	stsName := rmq.ChildResourceName("server")
	current, err := r.statefulSet(ctx, rmq)
	if err != nil {
		return err
	}
	if err := r.Delete(ctx, current, deleteOptions); err != nil {
		logger.Error(err, "failed to delete statefulSet", "statefulSet", stsName)
		return err
	}

	if err := retryWithInterval(logger, "delete statefulSet", 10, 3*time.Second, func() bool {
		_, getErr := r.statefulSet(ctx, rmq)
		if k8serrors.IsNotFound(getErr) {
			return true
		}
		return false
	}); err != nil {
		logger.Error(err, "statefulSet not deleting after 50 seconds", "statefulSet", stsName)
		return err
	}
	logger.Info("statefulSet deleted", "statefulSet", stsName)
	return nil
}

func retryWithInterval(logger logr.Logger, msg string, retry int, interval time.Duration, f func() bool) (err error) {
	for i := 0; i < retry; i++ {
		if ok := f(); ok {
			return
		}
		time.Sleep(interval)
		logger.Info("retrying again", "action", msg, "interval", interval, "attempt", i+1)
	}
	return fmt.Errorf("failed to %s after %d retries", msg, retry)
}
