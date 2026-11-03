import os

TOP_K = int(os.environ.get("TOP_K", "4"))
MAX_UPLOAD = int(os.environ.get("MAX_UPLOAD_BYTES", str(5 << 20)))
