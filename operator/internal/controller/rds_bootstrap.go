package controller

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	proxysqlv1alpha1 "github.com/ProxySQL/kubernetes/operator/api/v1alpha1"
	"github.com/ProxySQL/kubernetes/operator/internal/controller/builders"
	"github.com/ProxySQL/kubernetes/operator/internal/rdsclient"
)

const (
	rdsAccessKeyIDKey     = "access-key-id"
	rdsSecretAccessKeyKey = "secret-access-key"
	awsRegionAnnotation   = "proxysql.com/aws-region"
)

type rdsCredentialFile struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
}

// resolveAWSBootstrap verifies the selected opaque ConfigMap key and records
// only transport metadata needed by the StatefulSet and internal RDS client.
// It deliberately does not decode the deployment JSON.
func (r *ProxySQLClusterReconciler) resolveAWSBootstrap(ctx context.Context, cluster *proxysqlv1alpha1.ProxySQLCluster, b *builders.Builder) error {
	if !cluster.Spec.AWSPluginEnabled() {
		return nil
	}
	ref := cluster.Spec.Plugin.AWS.Bootstrap.ConfigMapRef
	if ref.Name == "" || ref.Key == "" {
		return fmt.Errorf("spec.plugin.aws.bootstrap.configMapRef name and key are required")
	}
	var configMap corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: cluster.Namespace}, &configMap); err != nil {
		return fmt.Errorf("read AWS bootstrap ConfigMap: %w", err)
	}
	value, ok := configMap.Data[ref.Key]
	if !ok {
		return fmt.Errorf("AWS bootstrap ConfigMap %q does not contain key %q", ref.Name, ref.Key)
	}
	if configMap.Annotations[awsRegionAnnotation] == "" {
		return fmt.Errorf("AWS bootstrap ConfigMap %q does not declare %s", ref.Name, awsRegionAnnotation)
	}
	b.AWSBootstrapChecksum = builders.CnfChecksum(map[string][]byte{ref.Key: []byte(value)})
	_, err := ensureRDSCredentials(ctx, r.Client, r.Scheme, cluster, b)
	return err
}

func ensureRDSCredentials(ctx context.Context, c client.Client, scheme *runtime.Scheme, cluster *proxysqlv1alpha1.ProxySQLCluster, b *builders.Builder) (rdsclient.Credentials, error) {
	key := types.NamespacedName{Name: b.AWSCredentialsSecretName(), Namespace: cluster.Namespace}
	existing := &corev1.Secret{}
	if err := c.Get(ctx, key, existing); err == nil {
		credentials, err := credentialsFromSecret(existing)
		if err != nil {
			return rdsclient.Credentials{}, fmt.Errorf("RDS API credential secret %q is incomplete: %w", key.Name, err)
		}
		encoded, err := encodeCredentialFile(credentials)
		if err != nil {
			return rdsclient.Credentials{}, err
		}
		if string(existing.Data[builders.AWSCredentialsFileName]) != string(encoded) {
			if existing.Data == nil {
				existing.Data = map[string][]byte{}
			}
			existing.Data[rdsAccessKeyIDKey] = []byte(credentials.AccessKeyID)
			existing.Data[rdsSecretAccessKeyKey] = []byte(credentials.SecretAccessKey)
			existing.Data[builders.AWSCredentialsFileName] = encoded
			if err := c.Update(ctx, existing); err != nil {
				return rdsclient.Credentials{}, fmt.Errorf("update RDS API credential secret: %w", err)
			}
		}
		return credentials, nil
	} else if !apierrors.IsNotFound(err) {
		return rdsclient.Credentials{}, fmt.Errorf("read RDS API credential secret: %w", err)
	}

	accessRandom, err := builders.RandomPassword()
	if err != nil {
		return rdsclient.Credentials{}, fmt.Errorf("generate RDS API access key: %w", err)
	}
	secret, err := builders.RandomPassword()
	if err != nil {
		return rdsclient.Credentials{}, fmt.Errorf("generate RDS API secret key: %w", err)
	}
	secretSuffix, err := builders.RandomPassword()
	if err != nil {
		return rdsclient.Credentials{}, fmt.Errorf("generate RDS API secret key: %w", err)
	}
	credentials := rdsclient.Credentials{AccessKeyID: "PX" + accessRandom, SecretAccessKey: secret + secretSuffix}
	encoded, err := encodeCredentialFile(credentials)
	if err != nil {
		return rdsclient.Credentials{}, err
	}
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: b.Labels()},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			rdsAccessKeyIDKey: []byte(credentials.AccessKeyID), rdsSecretAccessKeyKey: []byte(credentials.SecretAccessKey),
			builders.AWSCredentialsFileName: encoded,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, desired, scheme); err != nil {
		return rdsclient.Credentials{}, err
	}
	if err := c.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ensureRDSCredentials(ctx, c, scheme, cluster, b)
		}
		return rdsclient.Credentials{}, fmt.Errorf("create RDS API credential secret: %w", err)
	}
	return credentials, nil
}

func credentialsFromSecret(secret *corev1.Secret) (rdsclient.Credentials, error) {
	credentials := rdsclient.Credentials{
		AccessKeyID: string(secret.Data[rdsAccessKeyIDKey]), SecretAccessKey: string(secret.Data[rdsSecretAccessKeyKey]),
	}
	if credentials.AccessKeyID != "" && credentials.SecretAccessKey != "" {
		return credentials, nil
	}
	var file rdsCredentialFile
	if err := json.Unmarshal(secret.Data[builders.AWSCredentialsFileName], &file); err != nil {
		return rdsclient.Credentials{}, err
	}
	if file.Version != 1 || file.AccessKeyID == "" || file.SecretAccessKey == "" {
		return rdsclient.Credentials{}, fmt.Errorf("invalid credential file")
	}
	return rdsclient.Credentials{AccessKeyID: file.AccessKeyID, SecretAccessKey: file.SecretAccessKey}, nil
}

func encodeCredentialFile(credentials rdsclient.Credentials) ([]byte, error) {
	encoded, err := json.Marshal(rdsCredentialFile{Version: 1, AccessKeyID: credentials.AccessKeyID, SecretAccessKey: credentials.SecretAccessKey})
	if err != nil {
		return nil, fmt.Errorf("encode RDS API credential file: %w", err)
	}
	return encoded, nil
}
