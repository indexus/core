# One CPU instance computing the embeddings of the continual-learning
# experiment, eu-west-3, in a VPC of its own with outbound internet only
# (Hugging Face, PyPI, download.pytorch.org): no inbound rule, no SSH key, no
# IAM role, no credentials or personal data on it. Results go to a private S3
# bucket through presigned PUT URLs valid 3 h. It destroys itself after TTL.
# Usage, from the core repository root: bash scripts/research/indexus-cl/aws/launch.sh
set -u
export AWS_REGION=eu-west-3 AWS_DEFAULT_REGION=eu-west-3
HERE=scripts/research/indexus-cl
R=ops/agent/exec-ncmx/state/cl${RUN:-}.env
TAG="{Key=Project,Value=indexus-cl-embed},{Key=Name,Value=cl-embed${RUN:-}}"
if [ -f "$R" ] && grep -q INSTANCE= "$R"; then echo "cl.env exists: clean up first"; cat "$R"; exit 4; fi
TYPES="${TYPES:-c7i.8xlarge c6i.8xlarge m7i.8xlarge}"
MAXPRICE="${MAXPRICE:-2.5}"
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
python3 -m py_compile "$HERE/embed.py" || { echo "pre-flight: embed.py does not compile"; exit 8; }
AMI=$(aws ssm get-parameter --name /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id --query Parameter.Value --output text)
echo "AMI $AMI"
D=$(mktemp -d)
BUCKET="indexus-cl-embed-$(date +%Y%m%d%H%M)-$(python3 -c 'import secrets; print(secrets.token_hex(3))')"
aws s3api create-bucket --bucket "$BUCKET" --create-bucket-configuration LocationConstraint=eu-west-3 >/dev/null || exit 9
echo "BUCKET=$BUCKET" > "$R"
aws s3api put-public-access-block --bucket "$BUCKET" --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws s3api put-bucket-tagging --bucket "$BUCKET" --tagging 'TagSet=[{Key=Project,Value=indexus-cl-embed}]'
python3 - "$BUCKET" > "$D/urls.txt" <<'PY'
import sys, boto3
s3 = boto3.client("s3", region_name="eu-west-3", endpoint_url="https://s3.eu-west-3.amazonaws.com")
for ds in ("huffpost", "agnews"):
    for enc in ("all-MiniLM-L6-v2", "bge-small-en-v1.5"):
        k = f"{ds}__{enc}.npz"
        print(k, s3.generate_presigned_url("put_object", Params={"Bucket": sys.argv[1], "Key": k}, ExpiresIn=10800))
PY
[ "$(wc -l < "$D/urls.txt")" -eq 4 ] || { echo "presign failed"; exit 9; }
SRC=$(base64 < "$HERE/embed.py" | tr -d '\n')
URLS=$(base64 < "$D/urls.txt" | tr -d '\n')
{ echo '#!/bin/bash'; echo "SRC='$SRC'"; echo "URLS='$URLS'"; echo "TTL='${TTL:-90}'"; tail -n +2 "$HERE/aws/userdata.sh"; } > "$D/ud.sh"
gzip -9 -c "$D/ud.sh" > "$D/ud.gz"
echo "user-data: $(wc -c < "$D/ud.sh") bytes, gzip $(wc -c < "$D/ud.gz")"
[ $(( $(wc -c < "$D/ud.gz") * 4 / 3 )) -lt 16384 ] || { echo "user-data too large"; exit 7; }
VPC=$(aws ec2 create-vpc --cidr-block 10.79.0.0/24 --tag-specifications "ResourceType=vpc,Tags=[$TAG]" --query Vpc.VpcId --output text)
echo "VPC=$VPC" >> "$R"
IGW=$(aws ec2 create-internet-gateway --tag-specifications "ResourceType=internet-gateway,Tags=[$TAG]" --query InternetGateway.InternetGatewayId --output text)
echo "IGW=$IGW" >> "$R"
aws ec2 attach-internet-gateway --internet-gateway-id "$IGW" --vpc-id "$VPC"
RT=$(aws ec2 describe-route-tables --filters Name=vpc-id,Values="$VPC" --query 'RouteTables[0].RouteTableId' --output text)
aws ec2 create-route --route-table-id "$RT" --destination-cidr-block 0.0.0.0/0 --gateway-id "$IGW" >/dev/null
INSTANCE=""
for az in eu-west-3a eu-west-3b eu-west-3c; do
  SUB=$(aws ec2 create-subnet --vpc-id "$VPC" --cidr-block 10.79.0.0/24 --availability-zone $az --tag-specifications "ResourceType=subnet,Tags=[$TAG]" --query Subnet.SubnetId --output text)
  echo "SUBNET=$SUB" >> "$R"
  for t in $TYPES; do
    INSTANCE=$(aws ec2 run-instances --image-id "$AMI" --instance-type $t \
      --network-interfaces "DeviceIndex=0,SubnetId=$SUB,AssociatePublicIpAddress=true" \
      --block-device-mappings "DeviceName=/dev/sda1,Ebs={VolumeSize=${DISK:-40},VolumeType=gp3,DeleteOnTermination=true}" \
      --instance-initiated-shutdown-behavior terminate --user-data "fileb://$D/ud.gz" \
      --metadata-options HttpTokens=required \
      --tag-specifications "ResourceType=instance,Tags=[$TAG]" "ResourceType=volume,Tags=[$TAG]" \
      --query 'Instances[0].InstanceId' --output text 2>"$D/err") && break
    echo "$t in $az: $(tail -1 "$D/err")"; INSTANCE=""
  done
  [ -n "$INSTANCE" ] && { echo "TYPE=$t" >> "$R"; break; }
  aws ec2 delete-subnet --subnet-id "$SUB" && sed -i.bak "/SUBNET=$SUB/d" "$R"
done
rm -rf "$D"
if [ -z "$INSTANCE" ]; then echo "no capacity"; exit 6; fi
echo "INSTANCE=$INSTANCE" >> "$R"
echo "LAUNCHED=$(date -u +%FT%TZ)" >> "$R"
grep -v '^SUBNET' "$R"
aws ec2 wait instance-running --instance-ids "$INSTANCE" && echo "instance running"
