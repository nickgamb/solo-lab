# Keycloak 26.7.4 + keycloak/keycloak PR #49998 (ID-JAG issuer, issue #48818),
# backported onto the 26.7.4 tag (patches/). Only keycloak-core and
# keycloak-services change, so only those two jars are rebuilt and replaced;
# everything else is the stock release image.
#
# When upstream merges #49998, delete this directory and point KC_IMAGE back
# at quay.io/keycloak/keycloak:<release>.
ARG KEYCLOAK_VERSION=26.7.4
FROM maven:3.9-eclipse-temurin-21@sha256:99e61abcff91a9b1333463bd8451fb18495d6eba9250ac66a338b518f8278320 AS build
ARG KEYCLOAK_VERSION
RUN git clone -q --depth 1 --branch ${KEYCLOAK_VERSION} https://github.com/keycloak/keycloak.git /src
WORKDIR /src
COPY patches/ /patches/
RUN git -c user.name=lab -c user.email=lab@solo.lab am -q /patches/*.patch
RUN --mount=type=cache,target=/root/.m2 \
    mvn -B -q -pl core,services -DskipTests -Dmaven.test.skip=true -Denforcer.skip=true install

FROM quay.io/keycloak/keycloak:${KEYCLOAK_VERSION}
ARG KEYCLOAK_VERSION
COPY --from=build /src/core/target/keycloak-core-${KEYCLOAK_VERSION}.jar \
     /opt/keycloak/lib/lib/main/org.keycloak.keycloak-core-${KEYCLOAK_VERSION}.jar
COPY --from=build /src/services/target/keycloak-services-${KEYCLOAK_VERSION}.jar \
     /opt/keycloak/lib/lib/main/org.keycloak.keycloak-services-${KEYCLOAK_VERSION}.jar
LABEL org.opencontainers.image.description="Keycloak ${KEYCLOAK_VERSION} + PR #49998 (ID-JAG issuer) backport"
