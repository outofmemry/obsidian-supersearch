#!/bin/sh
# Compile the Swift OCR/PDF helper next to the server binary (skipped when up to date: swiftc takes ~40s).
set -eu
cd "$(dirname "$0")"
[ server/supersearch-helper -nt helper/main.swift ] || swiftc -O -o server/supersearch-helper helper/main.swift
