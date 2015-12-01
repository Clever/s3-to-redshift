FROM google/debian:wheezy

RUN apt-get update -y && apt-get install -y ca-certificates
RUN apt-get update -y && apt-get install -y curl

RUN curl -L https://github.com/Clever/gearcmd/releases/download/v0.4.0/gearcmd-v0.4.0-linux-amd64.tar.gz | tar xz -C /usr/local/bin --strip-components 1

COPY build/s3-to-redshift /usr/bin/s3-to-redshift

CMD ["gearcmd", "--name", "s3-to-redshift", "--cmd", "s3-to-redshift", "--cmdtimeout",  "59m"]