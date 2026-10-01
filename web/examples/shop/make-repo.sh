#!/bin/sh
# Builds the example shop repository of the website: a base commit, then the
# candidate change to review. Dates and authors are fixed, so the commits have
# the same IDs on every machine.
#
#   ./make-repo.sh /tmp/shop
#   cd /tmp/shop && probe review HEAD~1..HEAD --ci
set -eu

here=$(cd "$(dirname "$0")" && pwd)
dest=${1:?usage: make-repo.sh DIR}
if [ -e "$dest" ]; then
	echo "$dest already exists" >&2
	exit 1
fi
mkdir -p "$dest"
cd "$dest"

export GIT_AUTHOR_NAME="Shop Developer" GIT_AUTHOR_EMAIL="dev@shop.example"
export GIT_COMMITTER_NAME="Shop Developer" GIT_COMMITTER_EMAIL="dev@shop.example"
git init -q -b main
git config core.autocrlf false

# commit SNAPSHOT DATE MESSAGE replaces the tree with SNAPSHOT and commits it.
commit() {
	find . -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
	cp -R "$here/$1/." .
	git add -A
	GIT_AUTHOR_DATE="$2" GIT_COMMITTER_DATE="$2" git commit -q -m "$3"
}

commit base "2026-09-01T09:00:00Z" "Shop: orders, refunds and authorization"
commit candidate "2026-09-02T09:00:00Z" "Let support issue refunds, add a restocking fee and catalog helpers

Support agents handle refunds now, so that admins are no longer paged for
them. Refunds of returned goods keep a restocking fee of 10%, at most 15.00.
The catalog gets price and slug helpers for the storefront."

git log --oneline
