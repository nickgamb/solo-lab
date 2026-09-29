# u4a's agent adapter (clients/agent-shim) as an image, instead of u4a's
# pip-install-at-startup. Build context: the uma4agents repo.
FROM python:3.12-slim@sha256:f77ac9e44ae96ef2c90b8053ea08c31f8be030f824196b0ae4db6d462c84e51f
RUN pip install --no-cache-dir mcp==2.2.0 httpx==0.28.1 "pyjwt[crypto]==2.15.1" cryptography==50.0.1
WORKDIR /shim
COPY lib/ /shim/lib/
COPY clients/agent-shim/shim.py /shim/shim.py
ENV PYTHONPATH=/shim/lib PYTHONUNBUFFERED=1
USER 65532
EXPOSE 9030
CMD ["python3", "/shim/shim.py"]
