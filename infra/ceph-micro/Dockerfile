# Ceph de un nodo, sólo para probar el store RBD de Glance Go. NO es un
# despliegue: autenticación desactivada, memstore (RAM) y réplica 1.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ceph-mon ceph-osd ceph-common librados-dev librbd-dev gcc libc6-dev ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY micro-osd.sh /usr/local/bin/micro-osd.sh
RUN chmod 0755 /usr/local/bin/micro-osd.sh
