#!/usr/bin/env python3
"""Example: OCR a folder of page images on the Mac pool, in batches.

Each map task OCRs a batch of pages; a reduce task merges the text in page
order. Uses only the Python standard library and the REST API, to show that
apps don't need the Go SDK.

    export UMM_SERVER=https://umm.example.com UMM_API_KEY=...   # app key
    python3 submit.py pages/ --batch 10 --out book.txt

Agent allowlist on the Macs (see run.py / merge.py in this folder):

    runtimes:
      ocr:   ["/usr/bin/python3", "/opt/umm-examples/ocr/run.py"]
      merge: ["/usr/bin/python3", "/opt/umm-examples/ocr/merge.py"]
"""
import argparse
import json
import os
import pathlib
import time
import urllib.request
import uuid

SERVER = os.environ["UMM_SERVER"].rstrip("/")
KEY = os.environ["UMM_API_KEY"]


def call(method, path, body=None, raw=None):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    req = urllib.request.Request(SERVER + path, data=data, method=method)
    req.add_header("Authorization", "Bearer " + KEY)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req) as r:
        payload = r.read()
        ctype = r.headers.get("Content-Type", "")
        return json.loads(payload) if "json" in ctype else payload


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("pages", type=pathlib.Path)
    ap.add_argument("--batch", type=int, default=10)
    ap.add_argument("--out", default="ocr.txt")
    args = ap.parse_args()

    pages = sorted(p for p in args.pages.iterdir() if p.suffix.lower() in {".png", ".jpg", ".jpeg", ".tif", ".tiff"})
    prefix = "ocr/" + uuid.uuid4().hex[:12]
    tasks, merge_inputs, merge_args = [], [], ["{out:text.txt}"]
    for b in range(0, len(pages), args.batch):
        batch = pages[b : b + args.batch]
        inputs = []
        for i, p in enumerate(batch):
            key = f"{prefix}/{p.name}"
            call("PUT", "/v1/blobs/" + key, raw=p.read_bytes())
            inputs.append({"name": f"page{i:04d}{p.suffix.lower()}", "blob": key})
        k = f"batch-{b // args.batch:04d}"
        tasks.append({
            "key": k,
            "runtime": "ocr",
            "args": ["{out:text.txt}"] + ["{in:%s}" % x["name"] for x in inputs],
            "inputs": inputs,
            "outputs": [{"name": "text.txt"}],
            "resources": {"ram_mb": 1024, "expected_seconds": 5 * len(batch)},
            "max_retries": 2,
        })
        merge_inputs.append({"name": k, "from": {"task": k, "output": "text.txt"}})
        merge_args.append("{in:%s}" % k)
    tasks.append({"key": "merge", "runtime": "merge", "args": merge_args,
                  "inputs": merge_inputs, "outputs": [{"name": "text.txt"}]})

    job = call("POST", "/v1/jobs", {"name": f"ocr {args.pages.name}", "tasks": tasks})
    print("submitted", job["id"], "with", len(tasks), "tasks")
    while True:
        job = call("GET", "/v1/jobs/" + job["id"])
        print(job["state"], job["counts"])
        if job["state"] in ("succeeded", "failed", "cancelled"):
            break
        time.sleep(3)
    if job["state"] != "succeeded":
        raise SystemExit("job " + job["state"])
    merge = next(t for t in job["tasks"] if t["key"] == "merge")
    pathlib.Path(args.out).write_bytes(call("GET", f"/v1/tasks/{merge['id']}/outputs/text.txt"))
    print("wrote", args.out)


if __name__ == "__main__":
    main()
