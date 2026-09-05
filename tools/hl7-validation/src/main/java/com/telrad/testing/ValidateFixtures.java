package com.telrad.testing;

import ca.uhn.hl7v2.DefaultHapiContext;
import ca.uhn.hl7v2.HL7Exception;
import ca.uhn.hl7v2.HapiContext;
import ca.uhn.hl7v2.conf.check.DefaultValidator;
import ca.uhn.hl7v2.conf.parser.ProfileParser;
import ca.uhn.hl7v2.conf.spec.RuntimeProfile;
import ca.uhn.hl7v2.model.Message;
import ca.uhn.hl7v2.util.Terser;

import java.io.IOException;
import java.io.OutputStream;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Arrays;
import java.util.Map;
import java.util.Set;

public final class ValidateFixtures {
    private static final Path SOURCE_ROOT = Path.of("../..").toAbsolutePath().normalize();
    private static final Path FIXTURE_ROOT = SOURCE_ROOT.resolve("cmd/telrad-relay/testdata/hl7");
    private static final Path PROFILE_ROOT = Path.of("src/main/resources").toAbsolutePath().normalize();

    private ValidateFixtures() {
    }

    public static void main(String[] args) throws Exception {
        String report = renderFixture(
            FIXTURE_ROOT.resolve("oru-r01.hl7"),
            Map.of("{{CONTROL_ID}}", "control-validation-1")
        );
        String accepted = renderFixture(
            FIXTURE_ROOT.resolve("ack.hl7"),
            Map.of(
                "{{ACK_CODE}}", "AA",
                "{{CONTROL_ID}}", "control-validation-1",
                "{{ACK_CONTROL_ID}}", "ack-validation-1"
            )
        );
        String rejected = renderFixture(
            FIXTURE_ROOT.resolve("ack.hl7"),
            Map.of(
                "{{ACK_CODE}}", "AE",
                "{{CONTROL_ID}}", "control-validation-1",
                "{{ACK_CONTROL_ID}}", "ack-validation-2"
            )
        );

        RuntimeProfile reportProfile = readProfile("oru-r01.xml");
        RuntimeProfile acknowledgementProfile = readProfile("ack.xml");
        try (HapiContext context = new DefaultHapiContext()) {
            context.getParserConfiguration().setValidating(false);
            validate(context, report, reportProfile, "ORU^R01 report");
            validateAcknowledgement(context, accepted, acknowledgementProfile, "AA");
            validateAcknowledgement(context, rejected, acknowledgementProfile, "AE");
            validateKnownBadReport(context, report, reportProfile);
        }

        System.out.println("Validated synthetic ORU^R01 and ACK fixtures with HAPI HL7 2.6.0");
    }

    private static RuntimeProfile readProfile(String name) throws Exception {
        String profile = Files.readString(PROFILE_ROOT.resolve(name), StandardCharsets.UTF_8);
        PrintStream originalError = System.err;
        try (PrintStream ignored = new PrintStream(OutputStream.nullOutputStream())) {
            // ProfileParser first asks Xerces for DTD validation, which prints a
            // misleading no-grammar error for schema-only profiles. Its bundled
            // XSD validation still runs afterward and throws on a real error.
            System.setErr(ignored);
            return new ProfileParser(true).parse(profile);
        } finally {
            System.setErr(originalError);
        }
    }

    private static Message validate(
        HapiContext context,
        String encoded,
        RuntimeProfile profile,
        String name
    ) throws Exception {
        Message message = context.getPipeParser().parse(encoded);
        HL7Exception[] errors = new DefaultValidator(context).validate(message, profile.getMessage());
        if (errors.length != 0) {
            throw new IllegalStateException(name + " failed conformance validation: " + Arrays.toString(errors));
        }
        return message;
    }

    private static void validateAcknowledgement(
        HapiContext context,
        String encoded,
        RuntimeProfile profile,
        String expectedCode
    ) throws Exception {
        Message message = validate(context, encoded, profile, expectedCode + " ACK");
        String code = new Terser(message).get("/MSA-1");
        if (!Set.of("AA", "AE", "AR").contains(code) || !expectedCode.equals(code)) {
            throw new IllegalStateException("unexpected acknowledgement code " + code);
        }
    }

    private static void validateKnownBadReport(
        HapiContext context,
        String validReport,
        RuntimeProfile profile
    ) throws Exception {
        String invalidReport = setField(validReport, "OBR", 7, "");
        invalidReport = setField(invalidReport, "OBX", 7, "mmol/L");
        invalidReport = setField(invalidReport, "OBX", 10, "F");
        invalidReport = setField(invalidReport, "OBX", 11, "");

        Message message = context.getPipeParser().parse(invalidReport);
        HL7Exception[] errors = new DefaultValidator(context).validate(message, profile.getMessage());
        requireFieldError(errors, "OBR", 7);
        requireFieldError(errors, "OBX", 7);
        requireFieldError(errors, "OBX", 10);
        requireFieldError(errors, "OBX", 11);
    }

    private static void requireFieldError(HL7Exception[] errors, String segment, int field) {
        boolean found = Arrays.stream(errors).anyMatch(error ->
            (error.getLocation() != null
                && segment.equals(error.getLocation().getSegmentName())
                && field == error.getLocation().getField())
                || error.getMessageWithoutLocation().contains("Field " + field + " in " + segment)
        );
        if (!found) {
            throw new IllegalStateException(
                "validator accepted known-bad " + segment + "-" + field + ": " + Arrays.toString(errors)
            );
        }
    }

    private static String setField(String message, String segmentName, int field, String value) {
        String[] segments = message.split("\\r", -1);
        for (int index = 0; index < segments.length; index++) {
            if (!segments[index].startsWith(segmentName + "|")) {
                continue;
            }
            String[] fields = segments[index].split("\\|", -1);
            if (fields.length <= field) {
                int previousLength = fields.length;
                fields = Arrays.copyOf(fields, field + 1);
                Arrays.fill(fields, previousLength, fields.length, "");
            }
            fields[field] = value;
            segments[index] = String.join("|", fields);
            return String.join("\r", segments);
        }
        throw new IllegalArgumentException("segment not found: " + segmentName);
    }

    private static String renderFixture(Path path, Map<String, String> replacements) throws IOException {
        String message = Files.readString(path, StandardCharsets.UTF_8).replace("\r\n", "\n");
        for (Map.Entry<String, String> replacement : replacements.entrySet()) {
            message = message.replace(replacement.getKey(), replacement.getValue());
        }
        if (message.contains("{{")) {
            throw new IllegalStateException("unresolved fixture placeholder in " + path);
        }
        return message.replaceAll("\\n$", "").replace('\n', '\r') + "\r";
    }
}
