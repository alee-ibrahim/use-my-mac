# Examples

These are optional and not part of the core. The router knows nothing about
video or OCR: apps split the work, the Macs run allowlisted programs.

| Example | Shows |
| --- | --- |
| [`video-transcode/`](video-transcode/main.go) | Go SDK. Split a video locally, encode chunks in parallel with VideoToolbox (`gpu: true`), concatenate on one Mac. |
| [`ocr-batch/`](ocr-batch/submit.py) | Plain REST from Python. OCR page images in batches, merge the text in page order. |
| [`hello.yaml`](hello.yaml) | `umm submit` job spec with local file inputs (uploaded by the CLI). |

Each Mac must allowlist the runtimes an example uses in its `agent.yaml`.
