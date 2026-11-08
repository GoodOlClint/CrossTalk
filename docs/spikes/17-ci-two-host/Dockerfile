FROM debian:bookworm-slim
COPY crosstalk spike17 /usr/local/bin/
RUN useradd -m -d /home/x x
USER x
ENV HOME=/home/x
WORKDIR /home/x
CMD ["sleep", "infinity"]
