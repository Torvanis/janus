#!/usr/bin/env python3
"""Validate a release tag and changelog, exporting immutable release metadata."""
import datetime
import os
from pathlib import Path
import re
import subprocess


def metadata(tag, changelog, commit_date):
    match = re.fullmatch(r"v([0-9]{4}\.([1-9]|1[0-2])\.[1-9][0-9]*)", tag)
    if not match:
        raise ValueError("Tag must be vYEAR.MONTH.RELEASE_NUMBER")
    version = match[1]
    section = re.search(r"^## \[" + re.escape(version) + r"\][^\n]*\n(.*?)(?=^## \[|\Z)", changelog, re.M | re.S)
    if not section or not section[1].strip():
        raise ValueError("Missing or empty release changelog section")
    # The inaugural release date is fixed even when rebuilding its snapshot.
    date = "2026-09-20" if version == "2026.9.1" else commit_date
    datetime.datetime.strptime(date, "%Y-%m-%d")
    return version, date, section[1].strip() + "\n"


def main():
    commit_date = subprocess.check_output(["git", "show", "-s", "--format=%cd", "--date=format-local:%Y-%m-%d", "HEAD"], env=dict(os.environ, TZ="UTC"), text=True).strip()
    version, date, notes = metadata(os.environ["RELEASE_TAG"], Path("CHANGELOG.md").read_text(), commit_date)
    Path("release-notes.md").write_text(notes)
    with open(os.environ["GITHUB_ENV"], "a") as output:
        output.write(f"VERSION={version}\nBUILD_DATE={date}\n")


if __name__ == "__main__":
    main()
