package controller

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
	"github.com/ProxySQL/kubernetes/operator/internal/rdsconfig"
)

const (
	rdsCredentialSecretSuffix = "-rds-api"
	rdsAccessKeyIDKey         = "access-key-id"
	rdsSecretAccessKeyKey     = "secret-access-key"
)

// ensureRDSBootstrap preserves one internal SigV4 key pair and renders the
// plugin's one-shot bootstrap Secret from the complete RDS document.
func (r *ProxySQLConfigReconciler) ensureRDSBootstrap(
	ctx context.Context,
	cfg *proxysqlv1alpha1.ProxySQLConfig,
	cluster *proxysqlv1alpha1.ProxySQLCluster,
	b *builders.Builder,
) error {
	if cfg.Spec.RDS == nil || cluster.Spec.RDS == nil {
		return nil
	}
	credentials, err := r.ensureRDSCredentials(ctx, cluster, b)
	if err != nil {
		return err
	}
	raw, err := rawRDSConfig(cfg.Spec.RDS.Config)
	if err != nil {
		return err
	}
	stableID := string(cluster.UID)
	if stableID == "" {
		stableID = cluster.Namespace + "/" + cluster.Name
	}
	bootstrap, err := rdsconfig.BuildBootstrap(
		raw,
		cluster.Namespace+"/"+cluster.Name,
		stableID,
		cluster.Spec.RDS.Region,
		cluster.Spec.RDS.AssumeRoleARN,
		cluster.Spec.RDS.ExternalID,
		cluster.Spec.RDS.VPCID,
		cluster.Name+"."+cluster.Namespace+".svc",
		credentials,
	)
	if err != nil {
		return err
	}
	desired := &corev1.Secret{
		Name:      cluster.Name + builders.RDSBootstrapSecretSuffix,
		Namespace: cluster.Namespace,
		Labels:    b.Labels(),
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{builders.RDSBootstrapFileName: bootstrap},
	}
	existing := &corev1.Secret{Name: desired.Name, Namespace: desired.Namespace}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		existing.Type = desired.Type
		existing.Data = desired.Data
		return controllerutil.SetControllerReference(cluster, existing, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensure RDS bootstrap secret: %w", err)
	}
	return nil
}

func (r *ProxySQLConfigReconciler) ensureRDSCredentials(
	ctx context.Context,
	cluster *proxysqlv1alpha1.ProxySQLCluster,
	b *builders.Builder,
) (rdsconfig.Credentials, error) {
	key := types.NamespacedName{Name: cluster.Name + rdsCredentialSecretSuffix, Namespace: cluster.Namespace}
	existing := &corev1.Secret{}
	if err := r.Get(ctx, key, existing); err == nil {
		credentials := rdsconfig.Credentials{
			AccessKeyID: string(existing.Data[rdsAccessKeyIDKey]), SecretAccessKey: string(existing.Data[rdsSecretAccessKeyKey]),
		}
		if credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" {
			return rdsconfig.Credentials{}, fmt.Errorf("RDS API credential secret %q is incomplete", key.Name)
		}
		return credentials, nil
	} else if !apierrors.IsNotFound(err) {
		return rdsconfig.Credentials{}, fmt.Errorf("read RDS API credential secret: %w", err)
	}

	accessRandom, err := builders.RandomPassword()
	if err != nil {
		return rdsconfig.Credentials{}, fmt.Errorf("generate RDS API access key: %w", err)
	}
	secret, err := builders.RandomPassword()
	if err != nil {
		return rdsconfig.Credentials{}, fmt.Errorf("generate RDS API secret key: %w", err)
	}
	secretSuffix, err := builders.RandomPassword()
	if err != nil {
		return rdsconfig.Credentials{}, fmt.Errorf("generate RDS API secret key: %w", err)
	}
	credentials := rdsconfig.Credentials{AccessKeyID: "PX" + accessRandom, SecretAccessKey: secret + secretSuffix}
	desired := &corev1.Secret{
		Name:      key.Name,
		Namespace: key.Namespace,
		Labels:    b.Labels(),
		Type:      corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			rdsAccessKeyIDKey: []byte(credentials.AccessKeyID), rdsSecretAccessKeyKey: []byte(credentials.SecretAccessKey),
		},
	}
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return rdsconfig.Credentials{}, err
	}
	if err := r.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A concurrent reconcile won. Read and validate the preserved winner.
			return r.ensureRDSCredentials(ctx, cluster, b)
		}
		return rdsconfig.Credentials{}, fmt.Errorf("create RDS API credential secret: %w", err)
	}
	return credentials, nil
}

func rawRDSConfig(value runtime.RawExtension) ([]byte, error) {
	if len(value.Raw) > 0 {
		return append([]byte(nil), value.Raw...), nil
	}
	if value.Object == nil {
		return nil, fmt.Errorf("RDS config document is empty")
	}
	encoded, err := json.Marshal(value.Object)
	if err != nil {
		return nil, fmt.Errorf("encode RDS config document: %w", err)
	}
	return encoded, nil
}
