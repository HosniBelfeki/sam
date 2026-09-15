FROM python:3.14-slim@sha256:cad9a2c871761c413caa6fdd6441c783451e740a48aaeba60ae62a8b53525ef6

WORKDIR /workspace
COPY ./requirements.txt /workspace/requirements.txt
RUN pip install -r requirements.txt
COPY ./agent.py ./client.py /workspace/

CMD ["python3", "/workspace/agent.py"]
