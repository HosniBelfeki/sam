FROM python:3.12-slim

WORKDIR /srv
COPY requirements.txt /srv/
RUN pip install --no-cache-dir -r requirements.txt
COPY agent.py /srv/

ENV GEMINI_API_KEY=<API_KEY>
ENV GEMINI_MODEL=models/gemini-3.5-flash-lite

CMD ["python3", "/srv/agent.py"]
