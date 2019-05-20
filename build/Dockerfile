FROM registry.access.redhat.com/ubi7/ubi-minimal:latest

ENV OPERATOR=/usr/local/bin/rocketeer-operator \
    USER_UID=1001 \
    USER_NAME=rocketeer-operator

# install operator binary
COPY build/_output/bin/rocketeer-operator ${OPERATOR}

COPY build/bin /usr/local/bin
RUN  /usr/local/bin/user_setup

ENTRYPOINT ["/usr/local/bin/entrypoint"]

USER ${USER_UID}
