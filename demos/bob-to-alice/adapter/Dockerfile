# u4a's agent adapter (clients/agent-shim) as an image, instead of u4a's
# pip-install-at-startup. Build context: the uma4agents repo.
FROM python:3.12-slim
RUN pip install --no-cache-dir "mcp>=2,<3" httpx "pyjwt[crypto]" cryptography
WORKDIR /shim
COPY lib/ /shim/lib/
COPY clients/agent-shim/shim.py /shim/shim.py
ENV PYTHONPATH=/shim/lib PYTHONUNBUFFERED=1
USER 65532
EXPOSE 9030
CMD ["python3", "/shim/shim.py"]
