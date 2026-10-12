package io.solo.lab.continuity;

import org.keycloak.Config;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;
import org.keycloak.services.resource.RealmResourceProvider;
import org.keycloak.services.resource.RealmResourceProviderFactory;

/** {@code /realms/{realm}/continuity-upstream}: see {@link UpstreamIdTokenProvider}. */
public class UpstreamIdTokenProviderFactory implements RealmResourceProviderFactory {
    public static final String ID = "continuity-upstream";

    @Override public String getId() { return ID; }
    @Override public RealmResourceProvider create(KeycloakSession session) { return new UpstreamIdTokenProvider(session); }
    @Override public void init(Config.Scope config) { }
    @Override public void postInit(KeycloakSessionFactory factory) { }
    @Override public void close() { }
}
