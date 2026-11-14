#!/bin/sh
# Applies (or removes) the bucket CORS rule that browsers need in order to read
# presigned responses from the storage provider.
#
# This is a one-shot setup task. It needs a key with PutBucketCors, so run it
# separately from the gateway, which only ever gets a read-only key.
#
# Environment:
#   S3_ENDPOINT            required, e.g. https://de-fra.i3storage.com
#   S3_BUCKET              required
#   S3_REGION              default us-east-1
#   S3_ACCESS_KEY_ID       required (falls back to AWS_ACCESS_KEY_ID)
#   S3_SECRET_ACCESS_KEY   required (falls back to AWS_SECRET_ACCESS_KEY)
#   ACTION                 apply (default), delete or show
#   CORS_ALLOWED_ORIGINS   default *
#   CORS_ALLOWED_METHODS   default GET,HEAD
#   CORS_ALLOWED_HEADERS   default *
#   CORS_EXPOSE_HEADERS    default ETag,Content-Length,Content-Range,Accept-Ranges
#   CORS_MAX_AGE_SECONDS   default 3600, empty omits the element
set -eu

die() {
	echo "error: $*" >&2
	exit 1
}

[ -n "${S3_BUCKET:-}" ] || die "S3_BUCKET is required"
[ -n "${S3_ENDPOINT:-}" ] || die "S3_ENDPOINT is required"

endpoint=$S3_ENDPOINT
case $endpoint in
*://*) ;;
*) endpoint="https://$endpoint" ;;
esac
region=${S3_REGION:-us-east-1}
action=${ACTION:-apply}

# Accept this project's variable names as well as the AWS CLI defaults.
if [ -z "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${S3_ACCESS_KEY_ID:-}" ]; then
	export AWS_ACCESS_KEY_ID=$S3_ACCESS_KEY_ID
fi
if [ -z "${AWS_SECRET_ACCESS_KEY:-}" ] && [ -n "${S3_SECRET_ACCESS_KEY:-}" ]; then
	export AWS_SECRET_ACCESS_KEY=$S3_SECRET_ACCESS_KEY
fi
[ -n "${AWS_ACCESS_KEY_ID:-}" ] || die "S3_ACCESS_KEY_ID is required"
[ -n "${AWS_SECRET_ACCESS_KEY:-}" ] || die "S3_SECRET_ACCESS_KEY is required"

command -v aws >/dev/null 2>&1 || die "aws CLI not found in PATH"

s3api() {
	AWS_PAGER= aws --endpoint-url "$endpoint" --region "$region" s3api "$@"
}

trim() {
	printf '%s' "$1" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'
}

json_escape() {
	printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

# json_list <comma separated list> -> "a","b"
json_list() {
	# The trailing newline matters: without it read() returns non-zero at EOF
	# and the final item would be dropped.
	printf '%s\n' "$1" | tr ',' '\n' | while IFS= read -r item; do
		item=$(trim "$item")
		if [ -n "$item" ]; then
			printf '"%s",' "$(json_escape "$item")"
		fi
	done | sed 's/,$//'
}

max_age=${CORS_MAX_AGE_SECONDS-3600}
if [ -n "$max_age" ]; then
	case $max_age in
	*[!0-9]*) die "CORS_MAX_AGE_SECONDS must be a number or empty" ;;
	esac
fi

# The CLI takes this parameter as a JSON structure, not as the XML the API uses
# on the wire.
rule_file=$(mktemp)
trap 'rm -f "$rule_file"' EXIT

{
	echo '{'
	echo '  "CORSRules": ['
	echo '    {'
	printf '      "AllowedOrigins": [%s],\n' "$(json_list "${CORS_ALLOWED_ORIGINS:-*}")"
	printf '      "AllowedMethods": [%s],\n' "$(json_list "${CORS_ALLOWED_METHODS:-GET,HEAD}")"
	printf '      "AllowedHeaders": [%s]' "$(json_list "${CORS_ALLOWED_HEADERS:-*}")"
	expose=$(json_list "${CORS_EXPOSE_HEADERS:-ETag,Content-Length,Content-Range,Accept-Ranges}")
	if [ -n "$expose" ]; then
		printf ',\n      "ExposeHeaders": [%s]' "$expose"
	fi
	if [ -n "$max_age" ]; then
		printf ',\n      "MaxAgeSeconds": %s' "$max_age"
	fi
	echo
	echo '    }'
	echo '  ]'
	echo '}'
} >"$rule_file"

case $action in
apply)
	echo "==> applying CORS rule to $endpoint/$S3_BUCKET"
	s3api put-bucket-cors --bucket "$S3_BUCKET" --cors-configuration "file://$rule_file"
	;;
delete)
	echo "==> removing CORS rule from $endpoint/$S3_BUCKET"
	s3api delete-bucket-cors --bucket "$S3_BUCKET"
	;;
show)
	;;
*)
	die "ACTION must be apply, delete or show, got '$action'"
	;;
esac

echo "==> current rule"
if ! s3api get-bucket-cors --bucket "$S3_BUCKET" --output json; then
	echo "(no CORS rule configured)"
fi

if [ "$action" = apply ] || [ "$action" = delete ]; then
	echo
	echo "Done. This container is disposable: the credentials it used are gone with it."
	echo "Keep the write-capable key out of the gateway deployment."
fi
