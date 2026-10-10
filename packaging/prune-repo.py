#!/usr/bin/python3
# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0
"""Delete all but the newest N versions of each package in an RPM directory.

Only the newest release of each of the N newest versions is kept, so a
release bump does not push the previous version out.

Usage: prune-repo.py --keep N DIR
"""

import argparse
import functools
import os
import sys

import rpm


def header(ts, path):
    with open(path, "rb") as f:
        return ts.hdrFromFdno(f.fileno())


def evr(hdr):
    epoch = hdr[rpm.RPMTAG_EPOCH]
    return (
        "0" if epoch is None else str(epoch),
        hdr[rpm.RPMTAG_VERSION],
        hdr[rpm.RPMTAG_RELEASE],
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--keep", type=int, required=True)
    parser.add_argument("dir")
    args = parser.parse_args()
    if args.keep < 1:
        parser.error("--keep must be at least 1")

    ts = rpm.TransactionSet()
    # Only names and versions are read; signatures are checked elsewhere.
    ts.setVSFlags(rpm._RPMVSF_NOSIGNATURES | rpm._RPMVSF_NODIGESTS)

    by_name = {}
    for entry in sorted(os.listdir(args.dir)):
        if not entry.endswith(".rpm"):
            continue
        path = os.path.join(args.dir, entry)
        hdr = header(ts, path)
        key = (hdr[rpm.RPMTAG_NAME], hdr[rpm.RPMTAG_ARCH], bool(hdr[rpm.RPMTAG_SOURCEPACKAGE]))
        by_name.setdefault(key, []).append((evr(hdr), path))

    cmp = functools.cmp_to_key(lambda a, b: rpm.labelCompare(a[0], b[0]))
    for versions in by_name.values():
        versions.sort(key=cmp, reverse=True)
        newest = []
        for (epoch, version, _), path in versions:
            # Sorted newest first: the first build of a version is its newest.
            if (epoch, version) not in newest and len(newest) < args.keep:
                newest.append((epoch, version))
                continue
            print(f"pruning {path}", file=sys.stderr)
            os.remove(path)


if __name__ == "__main__":
    main()
