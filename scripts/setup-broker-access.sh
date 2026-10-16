#!/bin/sh
# Provision disposable LocalStack producer/worker credentials. Never uses AWS.
set -eu
for identity in wagering-producer wagering-worker; do
  docker compose exec -T localstack awslocal iam get-user --user-name "$identity" >/dev/null 2>&1 || docker compose exec -T localstack awslocal iam create-user --user-name "$identity" >/dev/null
  case "$identity" in
    wagering-producer) policy=deploy/aws/input-producer-policy.json ;;
    wagering-worker) policy=deploy/aws/worker-policy.json ;;
  esac
  docker compose exec -T localstack awslocal iam put-user-policy --user-name "$identity" --policy-name wagering-access --policy-document file:///dev/stdin < "$policy"
done
# Credentials are only written to the explicitly supplied destination, mode 600.
# Repeated runs do not create more keys unless requested.
if [ "$#" -eq 2 ]; then
  case "$1" in wagering-producer|wagering-worker) ;; *) exit 2 ;; esac
  (umask 077; set -C; docker compose exec -T localstack awslocal iam create-access-key --user-name "$1" > "$2")
fi
