clusterctl init --core cluster-api:v1.4.3 --bootstrap kubeadm:v1.4.3 --control-plane kubeadm:v1.4.3

oc adm policy add-scc-to-user privileged system:serviceaccount:capi-system:capi-manager

// make generate-go-apis
// RELEASE_TAG=dev make release-manifests
// make compiled-manifests   (not working?!)

//k apply -f out/infrastructure-components.yaml

k apply -f config/crd/bases/infrastructure.cluster.x-k8s.io_rosaclusters.yaml
k apply -f config/crd/bases/controlplane.cluster.x-k8s.io_rosacontrolplanes.yaml 

export OPENSHIFT_VERSION="openshift-v4.12.15"
export CLUSTER_NAME=mraee-capi-rosa
export AWS_REGION="us-west-2"
export AWS_ACCOUNT_ID="820196288204"
export AWS_CREATOR_ARN="arn:aws:iam::820196288204:user/mraee-dev"

cat templates/cluster-template-rosa.yaml | envsubst > rosa-capi-cluster.yaml


export OCM_TOKEN=
make manager-aws-infrastructure && ./bin/manager


##############

rosa create oidc-config --hosted-cp

rosa create operator-roles --prefix <user-defined> --oidc-config-id <id> --hosted-cp