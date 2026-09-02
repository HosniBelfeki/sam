FROM python:3.12-slim

WORKDIR /workspace
COPY ./requirements.txt /workspace/requirements.txt
RUN pip install -r requirements.txt
COPY ./agent.py ./client.py /workspace/

CMD ["python3", "/workspace/agent.py"]
