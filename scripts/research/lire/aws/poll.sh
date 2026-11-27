# Prints the [LIRE] lines of the instance console, and its state.
set -u
export AWS_REGION=eu-west-3 AWS_DEFAULT_REGION=eu-west-3
. ops/agent/exec-ncmx/state/lire${RUN:-}.env
aws ec2 describe-instances --instance-ids "$INSTANCE" --query 'Reservations[0].Instances[0].State.Name' --output text
aws ec2 get-console-output --instance-id "$INSTANCE" --latest --output text 2>/dev/null | grep -a "\[LIRE\]" | sed 's/^.*\[LIRE\] //'
