FROM python:3.12-slim@sha256:f77ac9e44ae96ef2c90b8053ea08c31f8be030f824196b0ae4db6d462c84e51f
RUN pip install --no-cache-dir mcp==2.2.0 uvicorn==0.54.0 "pyjwt[crypto]==2.15.1" cryptography==50.0.1
WORKDIR /app
COPY server.py book.json ./
USER 10001
EXPOSE 3000
CMD ["python", "server.py"]
