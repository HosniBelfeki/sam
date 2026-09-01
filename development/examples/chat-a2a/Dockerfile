FROM python:3.12-slim

WORKDIR /srv
COPY requirements.txt /srv/
RUN pip install --no-cache-dir -r requirements.txt
COPY agent.py /srv/

ENV GEMINI_API_KEY=<API_KEY>

CMD ["python3", "/srv/agent.py"]
