# syntax=docker/dockerfile:1
FROM docker.io/library/maven:3.9-eclipse-temurin-21 AS build
WORKDIR /src
COPY pom.xml .
RUN mvn -B -q dependency:go-offline
COPY src ./src
RUN mvn -B -q -DskipTests package

FROM docker.io/library/eclipse-temurin:21-jre
LABEL org.opencontainers.image.source="https://github.com/[[.Owner]]/[[.Repo]]"
# A numeric USER needs no passwd entry (and uid 1000 already exists as "ubuntu" in this base image).
COPY --from=build /src/target/app.jar /app/app.jar
USER [[.UID]]:[[.UID]]
EXPOSE [[.Port]]
ENV JAVA_TOOL_OPTIONS="-XX:MaxRAMPercentage=70 -Djava.io.tmpdir=/tmp"
ENTRYPOINT ["java", "-jar", "/app/app.jar"]
