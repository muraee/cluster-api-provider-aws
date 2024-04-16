package v1beta2

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/hash"
)

// SetDefaults_RosaControlPlaneSpec is used by defaulter-gen.
func SetDefaults_RosaControlPlaneSpec(s *RosaControlPlaneSpec) { //nolint:golint,stylecheck
	if s.IdentityRef == nil {
		s.IdentityRef = &v1beta2.AWSIdentityReference{
			Kind: v1beta2.ControllerIdentityKind,
			Name: v1beta2.AWSClusterControllerIdentityName,
		}
	}
}

const (
	clusterNamePrefix = "capa-"
)

// GenerateClusterName generates a default name for a ROSA cluster.
func GenerateClusterName(resourceName, namespace string, maxLength int) (string, error) {
	escapedName := strings.ReplaceAll(resourceName, ".", "-")
	clusterName := fmt.Sprintf("%s-%s", namespace, escapedName)

	if len(clusterName) < maxLength {
		return clusterName, nil
	}

	hashLength := 32 - len(clusterNamePrefix)
	hashedName, err := hash.Base36TruncatedHash(clusterName, hashLength)
	if err != nil {
		return "", errors.Wrap(err, "creating hash from name")
	}

	return fmt.Sprintf("%s%s", clusterNamePrefix, hashedName), nil
}
