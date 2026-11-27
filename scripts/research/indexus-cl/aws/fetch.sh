# Downloads the bucket into ../_work/datasets/cl and prints sha256 prefixes.
set -u
export AWS_REGION=eu-west-3 AWS_DEFAULT_REGION=eu-west-3
. ops/agent/exec-ncmx/state/cl${RUN:-}.env
OUT=../_work/datasets/cl; mkdir -p "$OUT"
aws s3 cp --recursive --only-show-errors "s3://$BUCKET/" "$OUT/"
for f in "$OUT"/*.npz; do echo "$(basename "$f") $(shasum -a 256 "$f" | cut -c1-16) $(stat -f %z "$f")"; done
