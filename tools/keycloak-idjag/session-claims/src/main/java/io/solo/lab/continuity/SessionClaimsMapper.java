package io.solo.lab.continuity;

import java.util.Collection;
import java.util.List;
import java.util.stream.Collectors;

import org.jboss.logging.Logger;
import org.keycloak.broker.oidc.mappers.AbstractClaimMapper;
import org.keycloak.broker.provider.AbstractIdentityProviderMapper;
import org.keycloak.broker.provider.BrokeredIdentityContext;
import org.keycloak.models.IdentityProviderMapperModel;
import org.keycloak.models.IdentityProviderSyncMode;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserModel;
import org.keycloak.provider.ProviderConfigProperty;

/**
 * Copies how the upstream IdP authenticated the user (its acr, amr and
 * auth_time) onto the broker's user session, as session notes named
 * {@code <prefix><claim>}. A session note lives and dies with that one
 * session, so what one sign-in proved never carries over to another.
 *
 * <p>Keycloak's own claim-to-session-note mapper takes string claims only;
 * this one also takes numbers (auth_time) and arrays (amr, joined with
 * spaces). A claim the upstream didn't send leaves no note: the broker's
 * tokens then say nothing for it, and whoever relies on it must treat that
 * as unproven.
 */
public class SessionClaimsMapper extends AbstractIdentityProviderMapper {
    public static final String PROVIDER_ID = "continuity-session-claims-idp-mapper";
    static final String CLAIMS = "claims";
    static final String PREFIX = "note.prefix";
    private static final Logger LOG = Logger.getLogger(SessionClaimsMapper.class);
    private static final String[] COMPATIBLE_PROVIDERS = {"oidc", "keycloak-oidc"};
    private static final List<ProviderConfigProperty> CONFIG = List.of(
            property(CLAIMS, "Claims", "Claim names, comma-separated (e.g. acr,amr,auth_time).", "acr,amr,auth_time"),
            property(PREFIX, "Session note prefix", "Prefix of each session note's name.", "continuity."));

    private static ProviderConfigProperty property(String name, String label, String help, String dflt) {
        ProviderConfigProperty p = new ProviderConfigProperty();
        p.setName(name);
        p.setLabel(label);
        p.setHelpText(help);
        p.setType(ProviderConfigProperty.STRING_TYPE);
        p.setDefaultValue(dflt);
        return p;
    }

    @Override public String getId() { return PROVIDER_ID; }
    @Override public String[] getCompatibleProviders() { return COMPATIBLE_PROVIDERS; }
    @Override public String getDisplayCategory() { return "User Session"; }
    @Override public String getDisplayType() { return "Upstream authentication to session"; }
    @Override public String getHelpText() {
        return "Copies the upstream's acr, amr and auth_time (strings, numbers or arrays) onto the user session, for a User Session Note protocol mapper.";
    }
    @Override public List<ProviderConfigProperty> getConfigProperties() { return CONFIG; }
    @Override public boolean supportsSyncMode(IdentityProviderSyncMode mode) { return true; }

    @Override
    public void importNewUser(KeycloakSession session, RealmModel realm, UserModel user, IdentityProviderMapperModel model, BrokeredIdentityContext context) {
        copy(model, context);
    }

    @Override
    public void updateBrokeredUser(KeycloakSession session, RealmModel realm, UserModel user, IdentityProviderMapperModel model, BrokeredIdentityContext context) {
        copy(model, context);
    }

    private void copy(IdentityProviderMapperModel model, BrokeredIdentityContext context) {
        String claims = model.getConfig().getOrDefault(CLAIMS, "acr,amr,auth_time");
        String prefix = model.getConfig().getOrDefault(PREFIX, "continuity.");
        for (String raw : claims.split(",")) {
            String claim = raw.trim();
            if (claim.isEmpty()) {
                continue;
            }
            String value = asString(AbstractClaimMapper.getClaimValue(context, claim));
            if (value == null) {
                continue;
            }
            context.setSessionNote(prefix + claim, value);
            LOG.debugf("session note %s%s for broker user %s", prefix, claim, context.getBrokerUserId());
        }
    }

    // a string as is; a number without a fraction as an integer; an array's
    // scalar values joined with spaces; anything else (objects) is left out
    static String asString(Object v) {
        if (v instanceof String s) {
            return s.isEmpty() ? null : s;
        }
        if (v instanceof Number n) {
            return n.doubleValue() == Math.rint(n.doubleValue()) ? Long.toString(n.longValue()) : n.toString();
        }
        if (v instanceof Boolean b) {
            return b.toString();
        }
        if (v instanceof Collection<?> c) {
            String joined = c.stream().map(SessionClaimsMapper::asString).filter(s -> s != null && !s.contains(" "))
                    .collect(Collectors.joining(" "));
            return joined.isEmpty() ? null : joined;
        }
        if (v instanceof com.fasterxml.jackson.databind.JsonNode n) {
            if (n.isTextual()) {
                return asString(n.asText());
            }
            if (n.isIntegralNumber()) {
                return Long.toString(n.asLong());
            }
            if (n.isArray()) {
                List<String> out = new java.util.ArrayList<>();
                n.forEach(e -> {
                    String s = e.isValueNode() ? asString(e.asText()) : null;
                    if (s != null && !s.contains(" ")) {
                        out.add(s);
                    }
                });
                return out.isEmpty() ? null : String.join(" ", out);
            }
        }
        return null;
    }
}
