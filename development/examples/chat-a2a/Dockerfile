FROM python:3.12-slim@sha256:78387bc3881b8273120a12ebe6c1ab22b018ccc2c9adf565ae1ac9b536e184ea

WORKDIR /srv
COPY requirements.txt /srv/
RUN pip install --no-cache-dir -r requirements.txt
COPY agent.py /srv/

ENV GEMINI_API_KEY=<API_KEY>
ENV GEMINI_MODEL=models/gemini-3.5-flash-lite

CMD ["python3", "/srv/agent.py"]
