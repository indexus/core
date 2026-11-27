# Prints the [CL] lines of the instance console, its state, and the bucket.
set -u
export AWS_REGION=eu-west-3 AWS_DEFAULT_REGION=eu-west-3
. ops/agent/exec-ncmx/state/cl${RUN:-}.env
aws ec2 describe-instances --instance-ids "$INSTANCE" --query 'Reservations[0].Instances[0].State.Name' --output text
aws ec2 get-console-output --instance-id "$INSTANCE" --latest --output text 2>/dev/null | grep -a "\[CL\]" | sed 's/^.*\[CL\] //'
aws s3 ls "s3://$BUCKET/"
