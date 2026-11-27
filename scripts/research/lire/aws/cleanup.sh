# Destroys everything launch.sh created, then checks that nothing is left.
set -u
export AWS_REGION=eu-west-3 AWS_DEFAULT_REGION=eu-west-3
R=ops/agent/exec-ncmx/state/lire${RUN:-}.env
[ -f "$R" ] && . "$R"
if [ -n "${INSTANCE:-}" ]; then
  aws ec2 terminate-instances --instance-ids "$INSTANCE" --query 'TerminatingInstances[0].CurrentState.Name' --output text
  aws ec2 wait instance-terminated --instance-ids "$INSTANCE" && echo "instance terminated"
fi
for s in $(grep '^SUBNET=' "$R" 2>/dev/null | cut -d= -f2); do aws ec2 delete-subnet --subnet-id "$s" && echo "subnet $s deleted"; done
if [ -n "${IGW:-}" ]; then aws ec2 detach-internet-gateway --internet-gateway-id "$IGW" --vpc-id "$VPC"; aws ec2 delete-internet-gateway --internet-gateway-id "$IGW" && echo "IGW deleted"; fi
[ -n "${VPC:-}" ] && aws ec2 delete-vpc --vpc-id "$VPC" && echo "VPC $VPC deleted"
echo "== check (tag Project=indexus-lire-bench, every run)"
echo "instances: $(aws ec2 describe-instances --filters Name=tag:Project,Values=indexus-lire-bench Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down --query 'length(Reservations[].Instances[])' --output text)"
echo "volumes:   $(aws ec2 describe-volumes --filters Name=tag:Project,Values=indexus-lire-bench --query 'length(Volumes)' --output text)"
echo "IGW:       $(aws ec2 describe-internet-gateways --filters Name=tag:Project,Values=indexus-lire-bench --query 'length(InternetGateways)' --output text)"
echo "VPC:       $(aws ec2 describe-vpcs --filters Name=tag:Project,Values=indexus-lire-bench --query 'length(Vpcs)' --output text)"
mv "$R" "$R.done-$(date +%Y%m%d-%H%M%S)" 2>/dev/null
