FROM python:3.14-slim@sha256:cad9a2c871761c413caa6fdd6441c783451e740a48aaeba60ae62a8b53525ef6

WORKDIR /srv
COPY requirements.txt /srv/
RUN pip install --no-cache-dir -r requirements.txt
COPY agent.py /srv/

ENV GEMINI_API_KEY=<API_KEY>
ENV GEMINI_MODEL=models/gemini-3.5-flash-lite

CMD ["python3", "/srv/agent.py"]
