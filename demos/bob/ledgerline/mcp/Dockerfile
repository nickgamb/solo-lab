FROM python:3.12-slim
RUN pip install --no-cache-dir "mcp>=2,<3" uvicorn "pyjwt[crypto]>=2.9,<3"
WORKDIR /app
COPY server.py ./
USER 10001
EXPOSE 3000
CMD ["python", "server.py"]
