# One CPU instance for the LIRE benchmark, eu-west-3, in a VPC of its own with
# outbound internet only (public datasets: Hugging Face, texmex): no inbound
# rule, no SSH key, no IAM role, no credentials or personal data on it. It
# shuts down and is destroyed by itself 200 minutes after boot at the latest.
# Usage, from the core repository root: bash <this> <dir of main.go, tree.go>
set -u
export AWS_REGION=eu-west-3 AWS_DEFAULT_REGION=eu-west-3
SRCDIR="$1"
R=ops/agent/exec-ncmx/state/lire${RUN:-}.env
TAG="{Key=Project,Value=indexus-lire-bench},{Key=Name,Value=lire-bench${RUN:-}}"
if [ -f "$R" ] && grep -q INSTANCE= "$R"; then echo "lire.env exists: clean up first"; cat "$R"; exit 4; fi
TYPES="${TYPES:-c7i.8xlarge c6i.8xlarge m7i.8xlarge}"
MAXPRICE="${MAXPRICE:-2.5}"
UD="${UD:-userdata.sh}"
for t in $TYPES; do
  p=$(aws pricing get-products --region us-east-1 --service-code AmazonEC2 --filters \
    Type=TERM_MATCH,Field=instanceType,Value=$t Type=TERM_MATCH,Field=regionCode,Value=eu-west-3 \
    Type=TERM_MATCH,Field=operatingSystem,Value=Linux Type=TERM_MATCH,Field=tenancy,Value=Shared \
    Type=TERM_MATCH,Field=preInstalledSw,Value=NA Type=TERM_MATCH,Field=capacitystatus,Value=Used \
    --query 'PriceList[0]' --output text 2>/dev/null | python3 -c "import sys,json; d=json.loads(sys.stdin.read()); t=list(d['terms']['OnDemand'].values())[0]; print(list(t['priceDimensions'].values())[0]['pricePerUnit']['USD'])" 2>/dev/null)
  echo "$t ${p:-?} USD/h"
  python3 -c "import sys; sys.exit(0 if float('${p:-99}') <= float('$MAXPRICE') else 1)" || { echo "$t above $MAXPRICE USD/h: not used"; TYPES=${TYPES/$t/}; }
done
[ -n "${TYPES// /}" ] || exit 5
AMI=$(aws ssm get-parameter --name /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id --query Parameter.Value --output text)
echo "AMI $AMI"
D=$(mktemp -d)
# The sources without their comment lines: user data is limited to 16 KiB.
mkdir -p "$D/src" && cp "$SRCDIR/go.mod" "$D/src/"
for f in main.go tree.go; do grep -v '^[[:space:]]*//' "$SRCDIR/$f" > "$D/src/$f"; done
SRC=$(tar czf - -C "$D/src" main.go tree.go go.mod | base64 | tr -d '\n')
# Pre-flight: the exact sources sent, built here and run on a small synthetic
# set with the requested options; nothing is created on AWS if this fails.
( cd "$D/src" && go build -o "$D/bench" . ) || { echo "pre-flight: build failed"; exit 8; }
for args in "${SIFT_ARGS:-}" "${BIG_ARGS:-}"; do
  "$D/bench" -n 3000 -q 20 $args > "$D/pre.txt" 2>&1 && grep -q "recall@10" "$D/pre.txt" || { echo "pre-flight failed with: $args"; tail -5 "$D/pre.txt"; exit 8; }
done
echo "pre-flight ok: sources $(cat "$D/src/main.go" "$D/src/tree.go" | shasum -a 256 | cut -c1-16)"
{ echo '#!/bin/bash'; echo "SRC='$SRC'"; echo "DATASET='${DATASET:-Qdrant/dbpedia-entities-openai3-text-embedding-3-large-3072-1M}'"; echo "SIFT='${SIFT:-1}' COLUMN='${COLUMN:-}' SIFT_ARGS='${SIFT_ARGS:-}' BIG_ARGS='${BIG_ARGS:-}' TTL='${TTL:-200}'"; tail -n +2 "$SRCDIR/aws/$UD"; } > "$D/ud.sh"
gzip -9 -c "$D/ud.sh" > "$D/ud.gz"
echo "user-data: $(wc -c < "$D/ud.sh") bytes, gzip $(wc -c < "$D/ud.gz") (limit 16384 before base64: $(( $(wc -c < "$D/ud.gz") * 4 / 3 )))"
[ $(( $(wc -c < "$D/ud.gz") * 4 / 3 )) -lt 16384 ] || { echo "user-data too large"; exit 7; }
VPC=$(aws ec2 create-vpc --cidr-block 10.78.0.0/24 --tag-specifications "ResourceType=vpc,Tags=[$TAG]" --query Vpc.VpcId --output text)
echo "VPC=$VPC" > "$R"
IGW=$(aws ec2 create-internet-gateway --tag-specifications "ResourceType=internet-gateway,Tags=[$TAG]" --query InternetGateway.InternetGatewayId --output text)
echo "IGW=$IGW" >> "$R"
aws ec2 attach-internet-gateway --internet-gateway-id "$IGW" --vpc-id "$VPC"
RT=$(aws ec2 describe-route-tables --filters Name=vpc-id,Values="$VPC" --query 'RouteTables[0].RouteTableId' --output text)
aws ec2 create-route --route-table-id "$RT" --destination-cidr-block 0.0.0.0/0 --gateway-id "$IGW" >/dev/null
INSTANCE=""
for az in eu-west-3a eu-west-3b eu-west-3c; do
  SUB=$(aws ec2 create-subnet --vpc-id "$VPC" --cidr-block 10.78.0.0/24 --availability-zone $az --tag-specifications "ResourceType=subnet,Tags=[$TAG]" --query Subnet.SubnetId --output text)
  echo "SUBNET=$SUB" >> "$R"
  for t in $TYPES; do
    INSTANCE=$(aws ec2 run-instances --image-id "$AMI" --instance-type $t \
      --network-interfaces "DeviceIndex=0,SubnetId=$SUB,AssociatePublicIpAddress=true" \
      --block-device-mappings "DeviceName=/dev/sda1,Ebs={VolumeSize=${DISK:-120},VolumeType=gp3,DeleteOnTermination=true}" \
      --instance-initiated-shutdown-behavior terminate --user-data "fileb://$D/ud.gz" \
      --metadata-options HttpTokens=required \
      --tag-specifications "ResourceType=instance,Tags=[$TAG]" "ResourceType=volume,Tags=[$TAG]" \
      --query 'Instances[0].InstanceId' --output text 2>"$D/err") && break
    echo "$t in $az: $(tail -1 "$D/err")"; INSTANCE=""
  done
  [ -n "$INSTANCE" ] && { echo "TYPE=$t" >> "$R"; break; }
  aws ec2 delete-subnet --subnet-id "$SUB" && sed -i.bak "/SUBNET=$SUB/d" "$R"
done
if [ -z "$INSTANCE" ]; then echo "no capacity"; exit 6; fi
echo "INSTANCE=$INSTANCE" >> "$R"
echo "LAUNCHED=$(date -u +%FT%TZ)" >> "$R"
cat "$R"
aws ec2 wait instance-running --instance-ids "$INSTANCE" && echo "instance running"
