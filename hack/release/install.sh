export VERSION=0-60e6caff30afbbaeec5de4b498bb89b073185654
export KARPENTER_IAM_ROLE_ARN="arn:aws:iam::${AWS_ACCOUNT_ID}:role/${CLUSTER_NAME}-karpenter"
aws ecr get-login-password --region us-west-2 | docker login --username AWS --password-stdin 953421922360.dkr.ecr.us-west-2.amazonaws.com
helm upgrade --install karpenter oci://953421922360.dkr.ecr.us-west-2.amazonaws.com/karpenter/karpenter --version "${VERSION}" --namespace "kube-system" --create-namespace \
  --set "settings.clusterName=${CLUSTER_NAME}" \
  --set controller.resources.requests.cpu=1 \
  --set controller.resources.requests.memory=20Gi \
  --set controller.resources.limits.cpu=1 \
  --set controller.resources.limits.memory=20Gi \
  --set serviceAccount.annotations.eks\\.amazonaws\\.com/role-arn=${KARPENTER_IAM_ROLE_ARN} \
  --wait